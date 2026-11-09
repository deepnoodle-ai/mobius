package mobius_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius"
	"github.com/deepnoodle-ai/mobius/mobius/action"
	"github.com/deepnoodle-ai/wonton/assert"
)

// stockComputerActions are the stock computer actions a worker runs, by the
// part of the name after "computer." / "environment.".
var stockComputerActions = []string{
	"bash",
	"files.read", "files.write", "files.list", "files.delete",
	"git.clone", "git.fetch", "git.status", "git.diff",
	"artifact.publish", "artifact.download",
	"logs.tail",
}

func newComputerWorker(t *testing.T, cfg mobius.WorkerConfig) *mobius.Worker {
	t.Helper()
	c, err := mobius.NewClient(mobius.WithBaseURL("https://api.example.invalid"), mobius.WithAPIKey("mbx_test"))
	assert.NoError(t, err)
	w := c.NewWorker(cfg)
	for _, a := range action.EnvironmentActions() {
		w.Register(a)
	}
	return w
}

// Servers before the Environment to Computer rename send environment.*;
// servers after it send computer.* to workers that advertise it. A worker
// advertises both.
func TestWorkerAdvertisesComputerAndEnvironmentActionNames(t *testing.T) {
	names := newComputerWorker(t, mobius.WorkerConfig{}).ClaimFrameActionNamesForTest()
	for _, suffix := range stockComputerActions {
		for _, prefix := range []string{"computer.", "environment."} {
			if !slices.Contains(names, prefix+suffix) {
				t.Errorf("advertised actions missing %s%s: %v", prefix, suffix, names)
			}
		}
	}
	assert.Equal(t, len(names), 2*len(stockComputerActions))
}

func TestWorkerRunsBashUnderEitherName(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", workspace)
	w := newComputerWorker(t, mobius.WorkerConfig{})
	for _, name := range []string{"computer.bash", "environment.bash"} {
		t.Run(name, func(t *testing.T) {
			marker := strings.ReplaceAll(name, ".", "_")
			out, err := w.RunActionJobForTest(name, map[string]any{
				"command": "printf ran > " + marker + " && printf ok",
			})
			assert.NoError(t, err)
			result, ok := out.(map[string]any)
			if !ok || result["stdout"] != "ok" {
				t.Fatalf("%s result = %#v, want stdout ok", name, out)
			}
			data, err := os.ReadFile(filepath.Join(workspace, marker))
			assert.NoError(t, err)
			assert.Equal(t, string(data), "ran")
		})
	}
}

// A job for a name the worker did not register is refused, not run; the
// refusal names the action the server sent.
func TestWorkerRefusesUnregisteredComputerAction(t *testing.T) {
	w := newComputerWorker(t, mobius.WorkerConfig{})
	_, err := w.RunActionJobForTest("computer.not_a_tool", nil)
	if err == nil || !strings.Contains(err.Error(), `action "computer.not_a_tool" not registered`) {
		t.Fatalf("err = %v, want not-registered refusal", err)
	}
}
