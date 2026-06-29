package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/dive/llm"
	"github.com/deepnoodle-ai/wonton/assert"
)

// sampleGenerationSpec mirrors the shape Mobius Cloud sends for an
// llm_generation job: the WorkerSocketLLMGenerationSpec envelope with the
// Dive/Anthropic request nested under "request" (alongside "route" and
// "mobius"), messages as content-block arrays, a system block array, and tool
// definitions.
func sampleGenerationSpec() map[string]any {
	return map[string]any{
		"route":  map[string]any{"mode": "worker", "provider": "ollama", "model": "llama3"},
		"mobius": map[string]any{"org_id": "org_1", "project_id": "proj_1"},
		"request": map[string]any{
			"model": "llama3",
			"messages": []map[string]any{
				{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}}},
			},
			"system":      []map[string]any{{"type": "text", "text": "be brief"}},
			"max_tokens":  float64(256),
			"temperature": float64(0.5),
			"tools": []map[string]any{
				{
					"name":         "get_weather",
					"description":  "Get the weather",
					"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
				},
			},
			"tool_choice": map[string]any{"type": "auto"},
		},
	}
}

func TestOllamaGenerateOptions_ReconstructsRequest(t *testing.T) {
	opts, err := ollamaGenerateOptions(sampleGenerationSpec())
	assert.NoError(t, err)

	var cfg llm.Config
	cfg.Apply(opts...)

	assert.Equal(t, 1, len(cfg.Messages))
	assert.Equal(t, llm.User, cfg.Messages[0].Role)
	text, ok := cfg.Messages[0].Content[0].(*llm.TextContent)
	assert.True(t, ok)
	assert.Equal(t, "hello", text.Text)

	assert.Equal(t, "be brief", cfg.SystemPrompt)
	assert.NotNil(t, cfg.MaxTokens)
	assert.Equal(t, 256, *cfg.MaxTokens)
	assert.NotNil(t, cfg.Temperature)
	assert.Equal(t, 0.5, *cfg.Temperature)

	assert.Equal(t, 1, len(cfg.Tools))
	assert.Equal(t, "get_weather", cfg.Tools[0].Name())
	assert.NotNil(t, cfg.ToolChoice)
	assert.Equal(t, llm.ToolChoiceTypeAuto, cfg.ToolChoice.Type)
}

func TestOllamaGenerateOptions_RequiresMessages(t *testing.T) {
	// Nested request with no messages.
	_, err := ollamaGenerateOptions(map[string]any{"request": map[string]any{"model": "llama3"}})
	assert.Error(t, err)

	// Nested request with empty messages.
	_, err = ollamaGenerateOptions(map[string]any{"request": map[string]any{"messages": []map[string]any{}}})
	assert.Error(t, err)

	// A spec missing the "request" envelope entirely must still fail clearly
	// rather than silently producing an empty generation.
	_, err = ollamaGenerateOptions(map[string]any{"route": map[string]any{"mode": "worker"}})
	assert.Error(t, err)
}

func TestSystemPromptText(t *testing.T) {
	// Plain string form.
	assert.Equal(t, "hi", systemPromptText("hi"))
	// Dive block-array form, joined.
	blocks := []map[string]any{
		{"type": "text", "text": "first"},
		{"type": "text", "text": "second"},
	}
	assert.Equal(t, "first\n\nsecond", systemPromptText(blocks))
}

func TestLLMResponseEnvelope_MatchesCloudContract(t *testing.T) {
	resp := &llm.Response{
		ID:         "msg_1",
		Model:      "llama3",
		Role:       llm.Assistant,
		Type:       "message",
		StopReason: "end_turn",
		Content:    []llm.Content{&llm.TextContent{Text: "hi"}},
		Usage:      llm.Usage{InputTokens: 3, OutputTokens: 2},
	}
	envelope, err := llmResponseEnvelope(resp)
	assert.NoError(t, err)

	// Mobius Cloud decodes result["llm_response"] as a Dive/Anthropic message
	// and requires id, type=message, role=assistant, model, content, stop_reason
	// and usage to be present.
	message, ok := envelope["llm_response"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "msg_1", message["id"])
	assert.Equal(t, "message", message["type"])
	assert.Equal(t, "assistant", message["role"])
	assert.Equal(t, "llama3", message["model"])
	assert.Equal(t, "end_turn", message["stop_reason"])
	content, ok := message["content"].([]any)
	assert.True(t, ok)
	assert.Equal(t, 1, len(content))
	_, ok = message["usage"].(map[string]any)
	assert.True(t, ok)
}

func TestDiscoverOllamaModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tags", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3"},{"name":"qwen2:7b"},{"name":""}]}`))
	}))
	defer srv.Close()

	models, err := discoverOllamaModels(context.Background(), srv.URL)
	assert.NoError(t, err)
	// Blank names are dropped.
	assert.Equal(t, 2, len(models))
	assert.Equal(t, "ollama", models[0].Provider)
	assert.Equal(t, "llama3", models[0].Model)
	assert.Equal(t, "qwen2:7b", models[1].Model)
}

func TestDiscoverOllamaModels_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := discoverOllamaModels(context.Background(), srv.URL)
	assert.Error(t, err)
}
