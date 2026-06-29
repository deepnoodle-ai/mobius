package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dive/llm"
	"github.com/deepnoodle-ai/dive/providers/ollama"
	"github.com/deepnoodle-ai/wonton/schema"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// ollamaProvider is the provider id local Ollama models are advertised and
// routed under. Mobius Cloud matches a model route on (provider, model)
// exactly, so an agent or loop step targeting a worker model uses
// {mode: "worker", provider: "ollama", model: <tag>}.
const ollamaProvider = "ollama"

// defaultOllamaURL is the base URL of a local Ollama server. The generation
// bridge talks to its Anthropic-compatible Messages API at <base>/v1/messages
// and discovers installed models at <base>/api/tags.
const defaultOllamaURL = "http://localhost:11434"

// discoverOllamaModels lists the models the local Ollama server currently has
// installed (its /api/tags endpoint), returning them as worker model
// capabilities to advertise to Mobius Cloud. A clear error is returned when the
// server is unreachable so an operator who passed --ollama learns immediately
// that nothing will be served.
func discoverOllamaModels(ctx context.Context, baseURL string) ([]mobius.ModelCapability, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach Ollama at %s: %w (is `ollama serve` running?)", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ollama tags request to %s returned %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Ollama tags from %s: %w", endpoint, err)
	}
	models := make([]mobius.ModelCapability, 0, len(payload.Models))
	for _, m := range payload.Models {
		if name := strings.TrimSpace(m.Name); name != "" {
			models = append(models, mobius.ModelCapability{Provider: ollamaProvider, Model: name})
		}
	}
	return models, nil
}

// ollamaModelNames returns just the model ids from a set of capabilities, for
// concise startup logging.
func ollamaModelNames(models []mobius.ModelCapability) []string {
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Model
	}
	return names
}

// newOllamaGenerator returns a worker generation handler that serves
// llm_generation jobs from a local Ollama server. It reconstructs the request
// from the job spec, streams text deltas back as they arrive, and returns the
// terminal response in the `llm_response` envelope Mobius Cloud expects. It is
// registered for the "*" model so it serves whatever model the server routes;
// the concrete installed models are advertised separately via discovery.
func newOllamaGenerator(baseURL string) mobius.GenerationFunc {
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/messages"
	return func(ctx mobius.Context, job mobius.GenerationJob, emit mobius.GenerationEmitter) (map[string]any, error) {
		if strings.TrimSpace(job.Model) == "" {
			return nil, fmt.Errorf("generation job is missing a model")
		}
		opts, err := ollamaGenerateOptions(job.Spec)
		if err != nil {
			return nil, fmt.Errorf("invalid generation spec: %w", err)
		}

		// The Ollama provider wraps Dive's Anthropic provider over Ollama's
		// Anthropic-compatible Messages API, so the response it returns is
		// already the Dive/Anthropic shape DecodeResult expects server-side.
		provider := ollama.New(ollama.WithEndpoint(endpoint), ollama.WithModel(job.Model))

		iter, err := provider.Stream(ctx, opts...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = iter.Close() }()

		acc := llm.NewResponseAccumulator()
		for iter.Next() {
			event := iter.Event()
			if err := acc.AddEvent(event); err != nil {
				return nil, err
			}
			// Forward live text deltas best-effort; the accumulated terminal
			// response below remains authoritative.
			if event.Type == llm.EventTypeContentBlockDelta &&
				event.Delta != nil &&
				event.Delta.Type == llm.EventDeltaTypeText &&
				event.Delta.Text != "" {
				_ = emit(map[string]any{"text": event.Delta.Text})
			}
		}
		if err := iter.Err(); err != nil {
			return nil, err
		}

		resp := acc.Response()
		if resp == nil {
			return nil, fmt.Errorf("ollama stream produced no response")
		}
		if resp.Model == "" {
			resp.Model = job.Model
		}
		return llmResponseEnvelope(resp)
	}
}

// llmResponseEnvelope wraps a Dive response in the result shape Mobius Cloud
// decodes for worker generations: {"llm_response": <Dive/Anthropic message>}.
func llmResponseEnvelope(resp *llm.Response) (map[string]any, error) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	var message map[string]any
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, err
	}
	return map[string]any{"llm_response": message}, nil
}

// ollamaGenerateOptions reconstructs Dive LLM options from a worker generation
// spec. The spec is the map the server produced from the agent's request, so
// the fields mirror Dive's request encoding. Only fields a local model can act
// on are mapped; provider-specific extras (metadata, stop sequences, reasoning
// budgets) are ignored.
func ollamaGenerateOptions(spec map[string]any) ([]llm.Option, error) {
	rawMessages, ok := spec["messages"]
	if !ok {
		return nil, fmt.Errorf("messages is required")
	}
	messages, err := decodeJSON[llm.Messages](rawMessages)
	if err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	opts := []llm.Option{llm.WithMessages(messages...)}

	if system, ok := spec["system"]; ok {
		if text := systemPromptText(system); text != "" {
			opts = append(opts, llm.WithSystemPrompt(text))
		}
	}
	if v, ok := spec["max_tokens"]; ok {
		if n, ok := toInt(v); ok {
			opts = append(opts, llm.WithMaxTokens(n))
		}
	}
	if v, ok := spec["temperature"]; ok {
		if f, ok := toFloat(v); ok {
			opts = append(opts, llm.WithTemperature(f))
		}
	}
	if v, ok := spec["reasoning_effort"]; ok {
		if s, ok := v.(string); ok && s != "" {
			if effort := llm.ReasoningEffort(s); effort.IsValid() {
				opts = append(opts, llm.WithReasoningEffort(effort))
			}
		}
	}
	if v, ok := spec["tools"]; ok {
		tools, err := decodeTools(v)
		if err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
		if len(tools) > 0 {
			opts = append(opts, llm.WithTools(tools...))
		}
	}
	if v, ok := spec["tool_choice"]; ok {
		tc, err := decodeJSON[*llm.ToolChoice](v)
		if err != nil {
			return nil, fmt.Errorf("tool_choice: %w", err)
		}
		if tc != nil && tc.Type != "" {
			opts = append(opts, llm.WithToolChoice(tc))
		}
	}
	return opts, nil
}

// systemPromptText extracts the system prompt from a spec value that is either a
// plain string or Dive's [{ "type": "text", "text": "..." }] block array.
func systemPromptText(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	blocks, err := decodeJSON[[]struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}](value)
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// decodeTools rebuilds Dive tool definitions from the spec's tool array
// ({ name, description, input_schema }).
func decodeTools(value any) ([]llm.Tool, error) {
	defs, err := decodeJSON[[]struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}](value)
	if err != nil {
		return nil, err
	}
	tools := make([]llm.Tool, 0, len(defs))
	for i, d := range defs {
		if strings.TrimSpace(d.Name) == "" {
			return nil, fmt.Errorf("tool[%d].name is required", i)
		}
		def := llm.NewToolDefinition().WithName(d.Name).WithDescription(d.Description)
		if len(d.InputSchema) > 0 {
			var s schema.Schema
			if err := json.Unmarshal(d.InputSchema, &s); err != nil {
				return nil, fmt.Errorf("tool[%d].input_schema: %w", i, err)
			}
			def = def.WithSchema(&s)
		}
		tools = append(tools, def)
	}
	return tools, nil
}

// decodeJSON re-marshals a decoded JSON value and unmarshals it into T. It lets
// the bridge reuse Dive's own (un)marshalers — notably the polymorphic message
// and content decoders — instead of hand-walking map[string]any.
func decodeJSON[T any](value any) (T, error) {
	var out T
	raw, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

// toInt coerces a JSON-decoded number (float64 by default) to an int.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// toFloat coerces a JSON-decoded number to a float64.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
