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
}

// openapiRoot is a minimal shape covering the fields we read.
type openapiRoot struct {
	Paths      map[string]map[string]specOpRaw `yaml:"paths"`
	Components specComponents                  `yaml:"components"`
}

// specComponents covers the reusable component blocks we resolve $ref against.
type specComponents struct {
	Parameters map[string]specParamRaw `yaml:"parameters"`
}

type specOpRaw struct {
	OperationID string         `yaml:"operationId"`
	Tags        []string       `yaml:"tags"`
	Summary     string         `yaml:"summary"`
	Description string         `yaml:"description"`
	Parameters  []specParamRaw `yaml:"parameters"`
}

type specParamRaw struct {
	// Ref is set when a parameter is a `$ref` into components/parameters
	// rather than an inline definition (the common case for path params).
	Ref         string `yaml:"$ref"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
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
	for p, methods := range root.Paths {
		for m, op := range methods {
			if op.OperationID == "" {
				continue
			}
			tag := ""
			if len(op.Tags) > 0 {
				tag = op.Tags[0]
			}
			paramDescs := make(map[string]string, len(op.Parameters))
			for _, p := range op.Parameters {
				// Path params are usually `$ref`s into components/parameters,
				// so resolve those to recover the param name + description.
				if p.Ref != "" {
					if rp, ok := root.Components.Parameters[refName(p.Ref)]; ok {
						p = rp
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
