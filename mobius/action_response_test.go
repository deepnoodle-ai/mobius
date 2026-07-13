package mobius

import (
	"encoding/json"
	"testing"
)

func TestActionResponseEnvelope(t *testing.T) {
	if ActionResponseContentType != "application/vnd.mobius.action+json" {
		t.Fatalf("content type = %q", ActionResponseContentType)
	}
	body, err := json.Marshal(ActionResponseEnvelope{
		Output:  map[string]any{"ok": true},
		Context: []RuntimeContextItem{{Name: "board", Content: "fresh"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), `{"output":{"ok":true},"context":[{"content":"fresh","name":"board"}]}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}
