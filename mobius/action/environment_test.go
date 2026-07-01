package action

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius"
)

type testActionContext struct {
	context.Context
}

func (testActionContext) Logger() *slog.Logger                               { return slog.Default() }
func (testActionContext) ProjectHandle() string                              { return "test-project" }
func (c testActionContext) ProjectID() string                                { return c.ProjectHandle() }
func (testActionContext) RunID() string                                      { return "run_test" }
func (testActionContext) JobID() string                                      { return "job_test" }
func (testActionContext) WorkflowName() string                               { return "" }
func (testActionContext) StepName() string                                   { return "step_test" }
func (testActionContext) Attempt() int                                       { return 1 }
func (testActionContext) Queue() string                                      { return "default" }
func (testActionContext) EmitEvent(eventType string, payload map[string]any) {}

func TestEnvironmentBashActionReportsTruncatedOutput(t *testing.T) {
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", realPath(t, t.TempDir()))
	out := executeEnvironmentAction(t, NewEnvironmentBashAction(), map[string]any{
		"command":          "printf 'abcdefghijklmnopqrstuvwxyz'",
		"max_output_bytes": 10,
	})

	stdout, _ := out["stdout"].(string)
	if !strings.HasPrefix(stdout, "abcdefghij") {
		t.Fatalf("stdout = %q, want truncated command output prefix", stdout)
	}
	if !strings.Contains(stdout, "partial stdout: truncated 16 bytes") {
		t.Fatalf("stdout missing visible truncation notice: %q", stdout)
	}
	if out["partial"] != true || out["truncated"] != true {
		t.Fatalf("truncation flags missing: %#v", out)
	}
	if out["stdout_truncated"] != true || out["stderr_truncated"] != false {
		t.Fatalf("stream truncation flags = %#v", out)
	}
	if out["stdout_bytes_omitted"] != 16 || out["bytes_omitted"] != 16 {
		t.Fatalf("omitted byte counts = %#v", out)
	}
	notice, _ := out["truncation_notice"].(string)
	if !strings.Contains(notice, "publish a large result as an artifact") {
		t.Fatalf("truncation notice missing bash guidance: %q", notice)
	}
}

func TestEnvironmentGitDiffActionUsesMaxOutputBytes(t *testing.T) {
	repo := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", repo)
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "notes.txt")
	runGit(t, repo, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte(strings.Repeat("changed line\n", 80)), 0o644); err != nil {
		t.Fatal(err)
	}

	out := executeEnvironmentAction(t, NewEnvironmentGitDiffAction(), map[string]any{
		"max_output_bytes": 80,
	})
	stdout, _ := out["stdout"].(string)
	if !strings.Contains(stdout, "diff --git") {
		t.Fatalf("stdout = %q, want git diff output", stdout)
	}
	if !strings.Contains(stdout, "partial stdout: truncated") {
		t.Fatalf("stdout missing visible truncation notice: %q", stdout)
	}
	if out["max_output_bytes"] != 80 {
		t.Fatalf("max_output_bytes = %#v, want 80", out["max_output_bytes"])
	}
	notice, _ := out["truncation_notice"].(string)
	if !strings.Contains(notice, "git diff --stat") {
		t.Fatalf("truncation notice missing diff guidance: %q", notice)
	}
}

func executeEnvironmentAction(t *testing.T, action interface {
	Execute(mobius.Context, map[string]any) (any, error)
}, params map[string]any) map[string]any {
	t.Helper()
	out, err := action.Execute(testActionContext{Context: context.Background()}, params)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output = %T, want map[string]any", out)
	}
	return result
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
