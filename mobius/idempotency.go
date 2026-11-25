package mobius

import (
	"context"
	"net/http"
	"strings"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

func normalizeIdempotencyKey(value string) string {
	return strings.TrimSpace(value)
}

func idempotencyRequestEditors(value string) []api.RequestEditorFn {
	key := normalizeIdempotencyKey(value)
	if key == "" {
		return nil
	}
	return []api.RequestEditorFn{func(_ context.Context, req *http.Request) error {
		req.Header.Set("Idempotency-Key", key)
		return nil
	}}
}
