package main

import (
	"fmt"
	"os"

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
	Paths map[string]map[string]specOpRaw `yaml:"paths"`
}

type specOpRaw struct {
	OperationID string         `yaml:"operationId"`
	Tags        []string       `yaml:"tags"`
	Summary     string         `yaml:"summary"`
	Description string         `yaml:"description"`
	Parameters  []specParamRaw `yaml:"parameters"`
}

type specParamRaw struct {
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
				if p.Description != "" {
					paramDescs[p.Name] = p.Description
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
