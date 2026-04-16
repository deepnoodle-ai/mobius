// Command mobius-cligen generates commands.gen.go for the mobius CLI from the
// OpenAPI spec and the oapi-codegen-generated client in mobius/api.
//
// It is invoked as a go:generate step (see ../mobius/app.go) and also via the
// Makefile's `generate-cli-commands` target.
//
// The generator walks every *ClientWithResponses method on the API client,
// matches it to an operationId in openapi.yaml, applies any overrides from
// overrides.go, and emits one wonton/cli command per operation.
//
// To suppress or override a command, edit the `overrides` map in
// overrides.go — never hand-edit the generated file.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	var (
		clientPath = flag.String("client", "api/client.gen.go", "path to the generated oapi-codegen client file")
		specPath   = flag.String("spec", "openapi.yaml", "path to the OpenAPI spec")
		outDir     = flag.String("out-dir", "cmd/mobius", "directory to write commands.gen.go and commands_<group>.gen.go into")
	)
	flag.Parse()

	if err := run(*clientPath, *specPath, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "mobius-cligen:", err)
		os.Exit(1)
	}
}

func run(clientPath, specPath, outDir string) error {
	client, err := parseClient(clientPath)
	if err != nil {
		return err
	}
	spec, err := parseSpec(specPath)
	if err != nil {
		return err
	}
	plan, warns := buildPlan(client, spec, overrides)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "  warn:", w)
	}
	files, err := render(plan, groupDescriptions)
	if err != nil {
		return err
	}

	// Prune any stale generated files from a previous run whose group no
	// longer exists (or whose filename style has since changed).
	if err := pruneStaleFiles(outDir, files); err != nil {
		return fmt.Errorf("prune stale: %w", err)
	}

	for name, src := range files {
		path := filepath.Join(outDir, name)
		if err := os.WriteFile(path, src, 0644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	fmt.Fprintf(os.Stderr, "mobius-cligen: wrote %d commands across %d files to %s\n",
		len(plan.Commands), len(files), outDir)
	return nil
}

// pruneStaleFiles removes commands*.gen.go files in outDir that are not in
// the current generation set. This keeps a rename of a group (or a newly
// suppressed group) from leaving an orphan file behind.
func pruneStaleFiles(outDir string, keep map[string][]byte) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "commands") || !strings.HasSuffix(name, ".gen.go") {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(outDir, name)); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "  removed stale:", name)
	}
	return nil
}

func lowerFirst(s string) string {
	if s == "" {
		return ""
	}
	return string(s[0]|0x20) + s[1:]
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func toKebab(s string) string {
	if s == "" {
		return ""
	}
	out := make([]byte, 0, len(s)+4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			if i > 0 && !isUpper(s[i-1]) {
				out = append(out, '-')
			} else if i > 0 && i+1 < len(s) && !isUpper(s[i+1]) && isUpper(s[i-1]) {
				// end of initialism run (e.g. "APIKey" -> "api-key")
				out = append(out, '-')
			}
			out = append(out, c|0x20)
		case c == '_' || c == ' ':
			out = append(out, '-')
		default:
			out = append(out, c)
		}
	}
	// Collapse any accidental double-dash.
	s2 := string(out)
	for {
		next := replaceAll(s2, "--", "-")
		if next == s2 {
			break
		}
		s2 = next
	}
	return s2
}

func isUpper(b byte) bool { return b >= 'A' && b <= 'Z' }

func replaceAll(s, old, new string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + new
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func groupVar(group string) string {
	out := make([]byte, 0, len(group))
	upper := false
	for i := 0; i < len(group); i++ {
		c := group[i]
		if c == '-' {
			upper = true
			continue
		}
		if upper {
			if c >= 'a' && c <= 'z' {
				c &^= 0x20
			}
			upper = false
		}
		out = append(out, c)
	}
	out = append(out, 'G', 'r', 'p')
	return string(out)
}
