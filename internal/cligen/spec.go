package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// SpecOp is the metadata we need from openapi.yaml for one operation.
type SpecOp struct {
	OperationID       string
	Path              string
	Method            string // GET, POST, PATCH, ...
	Tag               string // first tag, or derived from path
	Summary           string
	Description       string
	ParamDescriptions map[string]string // query/path param name → description
	// ParamDefaultTrue names the boolean query parameters whose schema
	// declares `default: true`. The server applies them when the parameter is
	// omitted, so the emitter pairs each one with a `--no-<flag>` switch —
	// otherwise the only way to turn the behavior off is `--flag=false`.
	ParamDefaultTrue map[string]bool
	// BodyRequired mirrors requestBody.required. When false (the OpenAPI
	// default), a bare invocation with no body is a valid request, so the
	// generator must not emit a "supply a flag or --file" guard.
	BodyRequired bool
	// NoAuth is true when the operation declares `security: []`: it takes no
	// credential, such as redeeming a pairing code. Its command must run on
	// a machine with no stored login.
	NoAuth bool
}

// openapiRoot is a minimal shape covering the fields we read.
type openapiRoot struct {
	// Each path item maps an HTTP method to its operation, plus an optional
	// path-level `parameters` list shared by every operation on the path, so
	// the values are decoded lazily in parseSpec.
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components specComponents                  `yaml:"components"`
}

// specComponents covers the reusable component blocks we resolve $ref against.
type specComponents struct {
	Parameters map[string]specParamRaw `yaml:"parameters"`
}

type specOpRaw struct {
	OperationID string              `yaml:"operationId"`
	Tags        []string            `yaml:"tags"`
	Summary     string              `yaml:"summary"`
	Description string              `yaml:"description"`
	Parameters  []specParamRaw      `yaml:"parameters"`
	RequestBody *specRequestBodyRaw `yaml:"requestBody"`
	// Security is nil when the operation inherits the spec's default and an
	// empty slice when it declares `security: []`.
	Security *[]map[string][]string `yaml:"security"`
}

// specRequestBodyRaw covers the one requestBody field the generator needs.
// An absent `required` key means false per the OpenAPI spec.
type specRequestBodyRaw struct {
	Required bool `yaml:"required"`
}

type specParamRaw struct {
	// Ref is set when a parameter is a `$ref` into components/parameters
	// rather than an inline definition (the common case for path params).
	Ref         string              `yaml:"$ref"`
	Name        string              `yaml:"name"`
	Description string              `yaml:"description"`
	Schema      *specParamSchemaRaw `yaml:"schema"`
}

// specParamSchemaRaw covers the parameter schema fields the generator reads.
// Default is `any` because it carries whatever the schema declares; only a
// boolean default is meaningful here.
type specParamSchemaRaw struct {
	Type    string `yaml:"type"`
	Default any    `yaml:"default"`
}

// parseSpec loads openapi.yaml and returns operations keyed by operationId.
func parseSpec(path string) (map[string]*SpecOp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var root openapiRoot
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]*SpecOp{}
	for p, item := range root.Paths {
		var pathParams []specParamRaw
		if node, ok := item["parameters"]; ok {
			if err := node.Decode(&pathParams); err != nil {
				return nil, fmt.Errorf("parse %s: path %s parameters: %w", path, p, err)
			}
		}
		for m, node := range item {
			if m == "parameters" || strings.HasPrefix(m, "x-") || m == "summary" || m == "description" || m == "servers" {
				continue
			}
			var op specOpRaw
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("parse %s: %s %s: %w", path, m, p, err)
			}
			if op.OperationID == "" {
				continue
			}
			// Path-level parameters apply to every operation on the path;
			// operation-level entries come last so their descriptions win.
			op.Parameters = append(append([]specParamRaw{}, pathParams...), op.Parameters...)
			tag := ""
			if len(op.Tags) > 0 {
				tag = op.Tags[0]
			}
			paramDescs := make(map[string]string, len(op.Parameters))
			defaultTrue := map[string]bool{}
			for _, p := range op.Parameters {
				// Path params are usually `$ref`s into components/parameters,
				// so resolve those to recover the param name + description.
				if p.Ref != "" {
					if rp, ok := root.Components.Parameters[refName(p.Ref)]; ok {
						p = rp
					}
				}
				if p.Name != "" && p.Schema != nil && p.Schema.Type == "boolean" {
					if b, ok := p.Schema.Default.(bool); ok && b {
						defaultTrue[normalizeParamKey(p.Name)] = true
					}
				}
				if p.Name != "" && p.Description != "" {
					// Normalize to lowercase-underscore so lookups keyed on the
					// kebab flag name match regardless of the param's original
					// casing or hyphens (e.g. header `X-Idempotency-Key`).
					paramDescs[normalizeParamKey(p.Name)] = p.Description
				}
			}
			out[op.OperationID] = &SpecOp{
				OperationID:       op.OperationID,
				Path:              p,
				Method:            m,
				Tag:               tag,
				Summary:           op.Summary,
				Description:       op.Description,
				ParamDescriptions: paramDescs,
				ParamDefaultTrue:  defaultTrue,
				BodyRequired:      op.RequestBody != nil && op.RequestBody.Required,
				NoAuth:            op.Security != nil && len(*op.Security) == 0,
			}
		}
	}
	return out, nil
}

// normalizeParamKey canonicalizes an OpenAPI parameter name to the same
// lowercase-underscore form the emitter derives from a CLI flag name, so
// descriptions match regardless of the param's original casing/hyphens.
func normalizeParamKey(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "-", "_")
}

// refName returns the component key a local `$ref` points at, i.e. the final
// path segment of e.g. "#/components/parameters/ActionNameParam".
func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
