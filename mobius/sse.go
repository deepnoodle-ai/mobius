package mobius

import (
	"context"
	"net/http"
)

// sseReadBufferSize bounds a single SSE line. A session frame can embed a
// whole tool result, so the bufio default (64KB) is too tight.
const sseReadBufferSize = 8 << 20

// acceptEventStream opts a request into Server-Sent Events. The streaming
// endpoints are content-negotiated and return a JSON page by default, so the
// stream is only opened when the request advertises text/event-stream.
func acceptEventStream(_ context.Context, req *http.Request) error {
	req.Header.Set("Accept", "text/event-stream")
	return nil
}
