package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius"
	"github.com/deepnoodle-ai/wonton/assert"
)

// These tests exercise the Ollama bridge against a real local server. They are
// opt-in so CI (which has no Ollama) skips them: set MOBIUS_TEST_OLLAMA=1 to
// run, and optionally MOBIUS_TEST_OLLAMA_MODEL to choose the model (defaults to
// a small installed one). Run with:
//
//	MOBIUS_TEST_OLLAMA=1 go test ./cmd/mobius -run TestOllamaLive -v

func ollamaLiveModel() string {
	if m := strings.TrimSpace(os.Getenv("MOBIUS_TEST_OLLAMA_MODEL")); m != "" {
		return m
	}
	return "llama3.2:latest"
}

func requireOllama(t *testing.T) {
	t.Helper()
	if os.Getenv("MOBIUS_TEST_OLLAMA") == "" {
		t.Skip("set MOBIUS_TEST_OLLAMA=1 to run live Ollama bridge tests")
	}
}

// testGenerationContext is a minimal mobius.Context for driving a generator
// outside the worker runtime. The Ollama bridge only uses it as a
// context.Context (for stream cancellation); the rest is identity metadata the
// handler doesn't read.
type testGenerationContext struct {
	context.Context
}

func (c testGenerationContext) Logger() *slog.Logger             { return slog.Default() }
func (c testGenerationContext) ProjectHandle() string            { return "test" }
func (c testGenerationContext) ProjectID() string                { return "test" }
func (c testGenerationContext) RunID() string                    { return "run_test" }
func (c testGenerationContext) JobID() string                    { return "job_test" }
func (c testGenerationContext) WorkflowName() string             { return "" }
func (c testGenerationContext) StepName() string                 { return "" }
func (c testGenerationContext) Attempt() int                     { return 1 }
func (c testGenerationContext) Queue() string                    { return "default" }
func (c testGenerationContext) EmitEvent(string, map[string]any) {}

func TestOllamaLive_Discovery(t *testing.T) {
	requireOllama(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	models, err := discoverOllamaModels(ctx, defaultOllamaURL)
	assert.NoError(t, err)
	if len(models) == 0 {
		t.Fatal("expected at least one installed Ollama model")
	}
	for _, m := range models {
		assert.Equal(t, ollamaProvider, m.Provider)
		assert.True(t, m.Model != "")
	}
	t.Logf("discovered %d models: %v", len(models), ollamaModelNames(models))
}

func TestOllamaLive_Generate(t *testing.T) {
	requireOllama(t)
	model := ollamaLiveModel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	gen := newOllamaGenerator(defaultOllamaURL)
	job := mobius.GenerationJob{
		JobID:    "job_test",
		Provider: ollamaProvider,
		Model:    model,
		Spec: map[string]any{
			"route": map[string]any{"mode": "worker", "provider": ollamaProvider, "model": model},
			"request": map[string]any{
				"model": model,
				"messages": []map[string]any{
					{"role": "user", "content": []map[string]any{
						{"type": "text", "text": "Reply with exactly the word: pong"},
					}},
				},
				"system":     []map[string]any{{"type": "text", "text": "You are a terse test fixture. Follow instructions exactly."}},
				"max_tokens": float64(64),
			},
		},
	}

	var (
		mu     sync.Mutex
		deltas []string
	)
	emit := func(delta map[string]any) error {
		if text, ok := delta["text"].(string); ok {
			mu.Lock()
			deltas = append(deltas, text)
			mu.Unlock()
		}
		return nil
	}

	result, err := gen(testGenerationContext{Context: ctx}, job, emit)
	assert.NoError(t, err)

	// Terminal result must be the cloud-contract envelope.
	message, ok := result["llm_response"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "message", message["type"])
	assert.Equal(t, "assistant", message["role"])
	assert.True(t, message["model"] != nil && message["model"] != "")

	content, ok := message["content"].([]any)
	assert.True(t, ok)
	assert.True(t, len(content) > 0)

	streamed := strings.Join(deltas, "")
	t.Logf("model=%s streamed %d delta(s), terminal stop_reason=%v", model, len(deltas), message["stop_reason"])
	t.Logf("streamed text: %q", streamed)

	// The accumulated terminal response should carry the same text the deltas
	// streamed (deltas are best-effort but the small prompt won't truncate).
	if streamed != "" {
		assert.True(t, strings.Contains(strings.ToLower(streamed), "pong"))
	}
}

func TestOllamaLive_GenerateWithTool(t *testing.T) {
	requireOllama(t)
	model := strings.TrimSpace(os.Getenv("MOBIUS_TEST_OLLAMA_TOOL_MODEL"))
	if model == "" {
		t.Skip("set MOBIUS_TEST_OLLAMA_TOOL_MODEL to a tool-capable model (e.g. qwen3:4b) to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	gen := newOllamaGenerator(defaultOllamaURL)
	job := mobius.GenerationJob{
		JobID:    "job_tool",
		Provider: ollamaProvider,
		Model:    model,
		Spec: map[string]any{
			"route": map[string]any{"mode": "worker", "provider": ollamaProvider, "model": model},
			"request": map[string]any{
				"model": model,
				"messages": []map[string]any{
					{"role": "user", "content": []map[string]any{
						{"type": "text", "text": "What is the weather in Paris? Use the tool."},
					}},
				},
				"max_tokens": float64(512),
				"tools": []map[string]any{
					{
						"name":        "get_weather",
						"description": "Get the current weather for a city.",
						"input_schema": map[string]any{
							"type":       "object",
							"properties": map[string]any{"city": map[string]any{"type": "string"}},
							"required":   []string{"city"},
						},
					},
				},
				"tool_choice": map[string]any{"type": "auto"},
			},
		},
	}

	result, err := gen(testGenerationContext{Context: ctx}, job, func(map[string]any) error { return nil })
	assert.NoError(t, err)

	message, ok := result["llm_response"].(map[string]any)
	assert.True(t, ok)
	content, ok := message["content"].([]any)
	assert.True(t, ok)

	var sawToolUse bool
	for _, c := range content {
		if block, ok := c.(map[string]any); ok && block["type"] == "tool_use" {
			sawToolUse = true
			t.Logf("tool_use: name=%v input=%v", block["name"], block["input"])
		}
	}
	t.Logf("model=%s stop_reason=%v sawToolUse=%v", model, message["stop_reason"], sawToolUse)
}
