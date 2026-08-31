package main

import (
	"github.com/deepnoodle-ai/wonton/cli"
)

// ResponseRenderer renders a successful response body for a specific
// operation. Implementations write to ctx.Stdout(); they are invoked only
// when the resolved --output mode is "pretty" (TTY default), so the
// canonical machine-parseable shape (json/yaml/id/name) is preserved for
// scripts.
//
// A renderer that decides the body is too unusual to handle nicely can call
// renderPretty(ctx, body) to fall through to the generic shape-driven path.
type ResponseRenderer func(ctx *cli.Context, body []byte) error

// responseRenderers maps OpenAPI operationId → custom pretty renderer. The
// generator looks the operationId up here before dispatching to the generic
// renderer; populate via RegisterResponseRenderer at process init.
var responseRenderers = map[string]ResponseRenderer{}

// RegisterResponseRenderer attaches a pretty renderer to one operation,
// keyed by its OpenAPI operationId (e.g. "getAgent", "listSessions"). Hand-
// written sibling files call this from init() to layer custom views on top
// of the generic table/key-value renderer without touching cligen.
func RegisterResponseRenderer(opID string, fn ResponseRenderer) {
	responseRenderers[opID] = fn
}

// responseRendererFor returns the renderer registered for opID, or nil if
// none is registered. Called by the generated printResponse runtime.
func responseRendererFor(opID string) ResponseRenderer {
	return responseRenderers[opID]
}
