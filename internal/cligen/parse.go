package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// ClientInfo is the subset of api/client.gen.go needed to generate CLI
// commands: method signatures on *ClientWithResponses, the Params structs they
// reference, and path-param type aliases.
type ClientInfo struct {
	Methods      map[string]*Method     // keyed by Go method name (without WithResponse suffix)
	ParamStructs map[string]*StructInfo // keyed by Go type name
	BodyStructs  map[string]*StructInfo // keyed by JSONRequestBody alias name (e.g. "CreateProjectJSONRequestBody")
	TypeAliases  map[string]string      // e.g. "IDParam" -> "string", "LimitParam" -> "int"
}

// Method captures one *ClientWithResponses method the generator may wrap.
type Method struct {
	Name      string  // e.g. "GetChannel" (without "WithResponse" suffix)
	Params    []Param // signature params, in order, excluding ctx and variadic editors
	ResponseT string  // e.g. "GetChannelResponse"
}

// Param is one positional method parameter.
type Param struct {
	Name string // argument name from the signature (e.g. "id", "params", "body")
	Type string // printed type expression (e.g. "IDParam", "*ListChannelsParams", "CreateChannelJSONRequestBody")
}

// StructInfo captures a struct type declaration.
type StructInfo struct {
	Name   string
	Fields []FieldInfo
}

// FieldInfo is one field of a query-params or request-body struct.
type FieldInfo struct {
	GoName  string // Go field name (PascalCase)
	Type    string // printed type expression (e.g. "*string", "*ListChannelsParamsKind")
	JSONTag string // the first segment of the json:"..." tag (e.g. "kind")
	Doc     string // doc-comment text (with leading field-name token stripped)
	Omit    bool   // true if the json tag included ",omitempty"
}

// parseClient reads api/client.gen.go and extracts the information the
// generator needs.
func parseClient(path string) (*ClientInfo, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	info := &ClientInfo{
		Methods:      map[string]*Method{},
		ParamStructs: map[string]*StructInfo{},
		BodyStructs:  map[string]*StructInfo{},
		TypeAliases:  map[string]string{},
	}

	// First pass: type decls. Collect aliases and every struct by name so that
	// a second pass can resolve JSONRequestBody aliases to their underlying
	// request struct.
	allStructs := map[string]*StructInfo{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			switch t := ts.Type.(type) {
			case *ast.Ident:
				// e.g. "type IDParam = string" (alias, ts.Assign != 0) or
				//      "type ListChannelsParamsKind string" (named defined type)
				info.TypeAliases[ts.Name.Name] = t.Name
			case *ast.StructType:
				si := &StructInfo{Name: ts.Name.Name}
				for _, f := range t.Fields.List {
					if len(f.Names) == 0 {
						continue
					}
					ftype, err := printExpr(fset, f.Type)
					if err != nil {
						return nil, err
					}
					jsonTag, omit := jsonTagKey(f.Tag)
					doc := fieldDoc(f)
					for _, n := range f.Names {
						si.Fields = append(si.Fields, FieldInfo{
							GoName:  n.Name,
							Type:    ftype,
							JSONTag: jsonTag,
							Omit:    omit,
							Doc:     stripLeadingDocIdent(doc, n.Name),
						})
					}
				}
				allStructs[ts.Name.Name] = si
				if strings.HasSuffix(ts.Name.Name, "Params") {
					info.ParamStructs[ts.Name.Name] = si
				}
			}
		}
	}

	// Resolve JSONRequestBody aliases (`type FooJSONRequestBody = FooRequest`)
	// to their underlying struct.
	for aliasName, target := range info.TypeAliases {
		if !strings.HasSuffix(aliasName, "JSONRequestBody") {
			continue
		}
		if si, ok := allStructs[target]; ok {
			info.BodyStructs[aliasName] = si
		}
	}

	// Second pass: method decls on *ClientWithResponses.
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		recv := fd.Recv.List[0].Type
		star, ok := recv.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "ClientWithResponses" {
			continue
		}
		name := fd.Name.Name
		if !strings.HasSuffix(name, "WithResponse") {
			continue
		}
		// Skip the "WithBody" raw-body variant — we use the typed JSON form.
		if strings.HasSuffix(name, "WithBodyWithResponse") {
			continue
		}
		base := strings.TrimSuffix(name, "WithResponse")
		m := &Method{
			Name:      base,
			ResponseT: base + "Response",
		}
		// Extract params: skip first (ctx) and any variadic RequestEditorFn.
		params := fd.Type.Params.List
		for i, p := range params {
			if i == 0 {
				continue // ctx
			}
			if _, isEllipsis := p.Type.(*ast.Ellipsis); isEllipsis {
				continue
			}
			ptype, err := printExpr(fset, p.Type)
			if err != nil {
				return nil, err
			}
			// p.Names may be empty for anonymous params (shouldn't happen here).
			for _, n := range p.Names {
				m.Params = append(m.Params, Param{Name: n.Name, Type: ptype})
			}
			if len(p.Names) == 0 {
				m.Params = append(m.Params, Param{Name: fmt.Sprintf("arg%d", i), Type: ptype})
			}
		}
		info.Methods[base] = m
	}

	return info, nil
}

func printExpr(fset *token.FileSet, e ast.Expr) (string, error) {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// jsonTagKey returns the first comma-delimited segment of a `json:"..."` tag
// and whether the tag included ",omitempty". Missing tag yields ("", false).
func jsonTagKey(lit *ast.BasicLit) (string, bool) {
	if lit == nil {
		return "", false
	}
	raw := strings.Trim(lit.Value, "`")
	const key = `json:"`
	idx := strings.Index(raw, key)
	if idx < 0 {
		return "", false
	}
	rest := raw[idx+len(key):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return "", false
	}
	val := rest[:end]
	name := val
	omit := false
	if comma := strings.Index(val, ","); comma >= 0 {
		name = val[:comma]
		if strings.Contains(val[comma:], "omitempty") {
			omit = true
		}
	}
	return name, omit
}

// fieldDoc returns the doc-comment text associated with a struct field, with
// leading "// " stripped and newlines collapsed to spaces. Both the block
// comment above the field (f.Doc) and the inline comment (f.Comment) are
// considered, in that order.
func fieldDoc(f *ast.Field) string {
	var parts []string
	if f.Doc != nil {
		for _, c := range f.Doc.List {
			parts = append(parts, strings.TrimSpace(strings.TrimPrefix(c.Text, "//")))
		}
	}
	if f.Comment != nil {
		for _, c := range f.Comment.List {
			parts = append(parts, strings.TrimSpace(strings.TrimPrefix(c.Text, "//")))
		}
	}
	return strings.Join(parts, " ")
}

// stripLeadingDocIdent removes a leading "FieldName " token from a doc comment.
// oapi-codegen emits field comments in the form "FieldName <description>"; we
// surface only the description. Returns the input unchanged when the prefix
// doesn't match.
func stripLeadingDocIdent(doc, fieldName string) string {
	doc = strings.TrimSpace(doc)
	if doc == "" || fieldName == "" {
		return doc
	}
	if strings.HasPrefix(doc, fieldName+" ") {
		return strings.TrimSpace(doc[len(fieldName)+1:])
	}
	if doc == fieldName {
		return ""
	}
	return doc
}
