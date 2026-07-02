package action

import (
	"context"
	"fmt"
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

// Bash: head+tail truncation with a prepended, self-describing notice and no
// parallel structured truncation fields (the agent is the only consumer).
func TestEnvironmentBashActionHeadTailTruncatesOutput(t *testing.T) {
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", realPath(t, t.TempDir()))
	out := executeEnvironmentAction(t, NewEnvironmentBashAction(), map[string]any{
		"command":          "printf 'abcdefghijklmnopqrstuvwxyz'",
		"max_output_bytes": 10,
	})

	stdout, _ := out["stdout"].(string)
	if !strings.HasPrefix(stdout, "[stdout truncated:") {
		t.Fatalf("stdout should lead with the truncation notice: %q", stdout)
	}
	if !strings.Contains(stdout, "16 B omitted") {
		t.Fatalf("stdout missing omitted-byte count: %q", stdout)
	}
	// Head is kept at the front, tail at the back, with a marker between them.
	if !strings.Contains(stdout, "abcde") || !strings.Contains(stdout, "vwxyz") {
		t.Fatalf("stdout should keep both head and tail: %q", stdout)
	}
	if strings.Index(stdout, "abcde") > strings.Index(stdout, "vwxyz") {
		t.Fatalf("head should precede tail: %q", stdout)
	}
	if !strings.Contains(stdout, "publish it as an artifact") {
		t.Fatalf("stdout missing bash recovery hint: %q", stdout)
	}
	if out["exit_code"] != 0 {
		t.Fatalf("exit_code = %#v, want 0", out["exit_code"])
	}
	// The removed structured contract must be gone.
	for _, k := range []string{"partial", "truncated", "bytes_omitted", "max_output_bytes", "stdout_truncated", "stdout_bytes_omitted", "truncation_notice"} {
		if _, ok := out[k]; ok {
			t.Fatalf("unexpected structured truncation key %q in %#v", k, out)
		}
	}
}

// Small output passes through byte-for-byte with no notice.
func TestEnvironmentBashActionKeepsSmallOutputIntact(t *testing.T) {
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", realPath(t, t.TempDir()))
	out := executeEnvironmentAction(t, NewEnvironmentBashAction(), map[string]any{
		"command": "printf 'hello world'",
	})
	if got := out["stdout"]; got != "hello world" {
		t.Fatalf("stdout = %#v, want exact passthrough", got)
	}
	if strings.Contains(fmt.Sprint(out["stdout"]), "truncated") {
		t.Fatalf("small output should carry no truncation notice: %#v", out)
	}
}

// git diff: a single oversized file falls back to head+tail on the bytes but
// still surfaces the diff header and the --stat recovery hint.
func TestEnvironmentGitDiffActionTruncatesSingleLargeFile(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "notes.txt", "one\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "initial")
	writeFile(t, repo, "notes.txt", strings.Repeat("changed line\n", 80))

	out := executeEnvironmentAction(t, NewEnvironmentGitDiffAction(), map[string]any{
		"max_output_bytes": 80,
	})
	stdout, _ := out["stdout"].(string)
	if !strings.HasPrefix(stdout, "[git diff truncated:") {
		t.Fatalf("stdout should lead with the diff truncation notice: %q", stdout)
	}
	if !strings.Contains(stdout, "diff --git") {
		t.Fatalf("stdout should still contain diff content: %q", stdout)
	}
	if !strings.Contains(stdout, "--stat") {
		t.Fatalf("stdout missing diff recovery hint: %q", stdout)
	}
}

// git diff: many changed files are truncated on whole-file boundaries, so the
// presented text stays a valid diff and the notice counts files, not bytes.
func TestEnvironmentGitDiffActionTruncatesOnFileBoundaries(t *testing.T) {
	repo := newTestRepo(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(t, repo, name, "x\n")
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "initial")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(t, repo, name, strings.Repeat("line\n", 40))
	}

	out := executeEnvironmentAction(t, NewEnvironmentGitDiffAction(), map[string]any{
		"max_output_bytes": 300,
	})
	stdout, _ := out["stdout"].(string)
	if !strings.Contains(stdout, "of 3 files") {
		t.Fatalf("notice should count files out of 3: %q", stdout)
	}
	body := stdout
	if i := strings.Index(stdout, "]\n\n"); i >= 0 {
		body = stdout[i+3:]
	}
	shown := strings.Count(body, "diff --git ")
	if shown < 1 || shown >= 3 {
		t.Fatalf("expected whole-file truncation (1-2 of 3 files), got %d: %q", shown, stdout)
	}
}

// git status: truncation is line-based (no mid-line cut) and counts lines.
func TestEnvironmentGitStatusActionTruncatesByLine(t *testing.T) {
	repo := newTestRepo(t)
	for i := 0; i < 60; i++ {
		writeFile(t, repo, fmt.Sprintf("file-%02d.txt", i), "x\n")
	}

	out := executeEnvironmentAction(t, NewEnvironmentGitStatusAction(), map[string]any{
		"max_output_bytes": 60,
	})
	stdout, _ := out["stdout"].(string)
	if !strings.HasPrefix(stdout, "[git status truncated: showing ") || !strings.Contains(stdout, "lines") {
		t.Fatalf("stdout missing line-based status notice: %q", stdout)
	}
	if !strings.Contains(stdout, "pathspec") {
		t.Fatalf("stdout missing status recovery hint: %q", stdout)
	}
	// Every kept status line must be complete (start with a status code).
	body := stdout[strings.Index(stdout, "]\n\n")+3:]
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if ln == "" {
			continue
		}
		if len(ln) < 2 {
			t.Fatalf("status line looks cut mid-line: %q", ln)
		}
	}
}

// A caller request above the absolute cap is clamped to absoluteOutputMaxBytes,
// which the override helper drives down so the clamp is observable.
func TestEnvironmentBashActionClampsRequestToAbsoluteCap(t *testing.T) {
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", realPath(t, t.TempDir()))
	setSizeForTest(t, &absoluteOutputMaxBytes, 8)
	out := executeEnvironmentAction(t, NewEnvironmentBashAction(), map[string]any{
		"command":          "printf 'abcdefghijklmnop'", // 16 bytes
		"max_output_bytes": 1000,                        // well above the absolute cap
	})
	stdout, _ := out["stdout"].(string)
	if !strings.HasPrefix(stdout, "[stdout truncated:") {
		t.Fatalf("request above the absolute cap should still truncate: %q", stdout)
	}
	if !strings.Contains(stdout, "kept first 4 B + last 4 B") {
		t.Fatalf("absolute cap of 8 should keep 4+4 bytes: %q", stdout)
	}
}

// files.read requests are clamped to the absolute cap too — an explicit oversized
// max_bytes cannot bypass the layer ceiling.
func TestEnvironmentFileReadActionClampsRequestToAbsoluteCap(t *testing.T) {
	root := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", root)
	body := strings.Repeat("0123456789", 30) // 300 bytes
	writeFile(t, root, "big.txt", body)
	setSizeForTest(t, &absoluteOutputMaxBytes, 8)

	out := executeEnvironmentAction(t, NewEnvironmentFileReadAction(), map[string]any{
		"path":      "big.txt",
		"max_bytes": 1_000_000, // far above the absolute cap
	})
	if out["truncated"] != true {
		t.Fatalf("oversized read request should be clamped and truncated: %#v", out)
	}
	if out["next_offset"] != int64(8) {
		t.Fatalf("next_offset = %#v, want 8 (clamped page size)", out["next_offset"])
	}
	content, _ := out["content"].(string)
	if !strings.HasPrefix(content, body[:8]) {
		t.Fatalf("content should start with the first 8 bytes: %q", content)
	}
}

// logs.tail requests are clamped to the absolute cap too.
func TestEnvironmentLogsTailActionClampsRequestToAbsoluteCap(t *testing.T) {
	logDir := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_WORKER_LOG_DIR", logDir)
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "line%03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(logDir, "worker.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	setSizeForTest(t, &absoluteOutputMaxBytes, 40)

	out := executeEnvironmentAction(t, NewEnvironmentLogsTailAction(), map[string]any{
		"max_bytes": 1_000_000, // far above the absolute cap
	})
	content, _ := out["content"].(string)
	if !strings.HasPrefix(content, "[logs truncated:") {
		t.Fatalf("oversized logs request should be clamped and truncated: %q", content)
	}
	if !strings.Contains(content, "line099") {
		t.Fatalf("clamped logs.tail should still keep the newest lines: %q", content)
	}
}

// Lowering capturedOutputCeiling forces structural truncation to report an
// approximate ("N+") total, proving the ceiling knob is wired through the tool.
func TestEnvironmentGitStatusActionCeilingMarksApproxTotal(t *testing.T) {
	repo := newTestRepo(t)
	for i := 0; i < 60; i++ {
		writeFile(t, repo, fmt.Sprintf("file-%02d.txt", i), "x\n")
	}
	setSizeForTest(t, &capturedOutputCeiling, 40)

	out := executeEnvironmentAction(t, NewEnvironmentGitStatusAction(), map[string]any{
		"max_output_bytes": 400, // above the ceiling, so the ceiling is the constraint
	})
	stdout, _ := out["stdout"].(string)
	if !strings.HasPrefix(stdout, "[git status truncated: showing ") {
		t.Fatalf("expected a status truncation notice: %q", stdout)
	}
	if !strings.Contains(stdout, "+ lines") {
		t.Fatalf("hitting the capture ceiling should mark the total approximate: %q", stdout)
	}
}

// files.read: oversized reads paginate — truncated content carries an in-band
// trailer and a next_offset the agent can pass back to continue.
func TestEnvironmentFileReadActionPaginatesWithOffset(t *testing.T) {
	root := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", root)
	body := strings.Repeat("0123456789", 30) // 300 bytes
	writeFile(t, root, "big.txt", body)

	out := executeEnvironmentAction(t, NewEnvironmentFileReadAction(), map[string]any{
		"path":      "big.txt",
		"max_bytes": 100,
	})
	if out["truncated"] != true {
		t.Fatalf("expected truncated read: %#v", out)
	}
	if out["next_offset"] != int64(100) {
		t.Fatalf("next_offset = %#v, want 100", out["next_offset"])
	}
	content, _ := out["content"].(string)
	if !strings.HasPrefix(content, body[:100]) {
		t.Fatalf("content should start with first 100 bytes: %q", content)
	}
	if !strings.Contains(content, "offset=100") {
		t.Fatalf("content missing pagination trailer: %q", content)
	}

	// Continue from the reported offset.
	out2 := executeEnvironmentAction(t, NewEnvironmentFileReadAction(), map[string]any{
		"path":      "big.txt",
		"max_bytes": 100,
		"offset":    int64(100),
	})
	content2, _ := out2["content"].(string)
	if !strings.HasPrefix(content2, body[100:200]) {
		t.Fatalf("second page should start at offset 100: %q", content2)
	}
}

// files.list: entries are ranked newest-first and the overflow is reported with
// an accurate total (not an off-by-one "looks full" guess).
func TestEnvironmentFileListRanksNewestAndReportsTotal(t *testing.T) {
	root := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", root)
	for i := 0; i < 5; i++ {
		writeFile(t, root, fmt.Sprintf("f%d.txt", i), "x")
	}

	out := executeEnvironmentAction(t, NewEnvironmentFileListAction(), map[string]any{
		"max_entries": 2,
	})
	if out["truncated"] != true {
		t.Fatalf("expected truncated listing: %#v", out)
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "of 5 entries") {
		t.Fatalf("note should report the true total: %q", note)
	}
	entries, _ := out["entries"].([]map[string]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

// files.list is byte-capped too: a large max_entries cannot push the serialized
// listing past the absolute cap.
func TestEnvironmentFileListClampsToAbsoluteCap(t *testing.T) {
	root := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", root)
	for i := 0; i < 10; i++ {
		writeFile(t, root, fmt.Sprintf("file-with-a-longish-name-%02d.txt", i), "x")
	}
	setSizeForTest(t, &absoluteOutputMaxBytes, 200)

	out := executeEnvironmentAction(t, NewEnvironmentFileListAction(), map[string]any{
		"max_entries": 100, // far more than the byte budget allows
	})
	if out["truncated"] != true {
		t.Fatalf("listing over the byte cap should be truncated: %#v", out)
	}
	entries, _ := out["entries"].([]map[string]any)
	if len(entries) < 1 || len(entries) >= 10 {
		t.Fatalf("byte cap should bind before max_entries (1-9 of 10), got %d", len(entries))
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "cap") {
		t.Fatalf("note should attribute truncation to the size cap: %q", note)
	}
}

// A listing that fits exactly must NOT report truncation (regression for the
// old `len(entries) >= maxEntries` false positive).
func TestEnvironmentFileListExactFitNotTruncated(t *testing.T) {
	root := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", root)
	writeFile(t, root, "a.txt", "x")
	writeFile(t, root, "b.txt", "x")

	out := executeEnvironmentAction(t, NewEnvironmentFileListAction(), map[string]any{
		"max_entries": 2,
	})
	if out["truncated"] != false {
		t.Fatalf("exact-fit listing should not be truncated: %#v", out)
	}
	if _, ok := out["note"]; ok {
		t.Fatalf("exact-fit listing should carry no note: %#v", out)
	}
}

// git diff: a diff that fits passes through verbatim with no notice.
func TestEnvironmentGitDiffActionSmallDiffPassthrough(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "notes.txt", "one\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "initial")
	writeFile(t, repo, "notes.txt", "two\n")

	out := executeEnvironmentAction(t, NewEnvironmentGitDiffAction(), map[string]any{})
	stdout, _ := out["stdout"].(string)
	if !strings.Contains(stdout, "diff --git") {
		t.Fatalf("stdout = %q, want git diff output", stdout)
	}
	if strings.Contains(stdout, "truncated") {
		t.Fatalf("small diff should carry no truncation notice: %q", stdout)
	}
}

// git status: a status that fits passes through verbatim with no notice.
func TestEnvironmentGitStatusActionSmallStatusPassthrough(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "only.txt", "x\n")

	out := executeEnvironmentAction(t, NewEnvironmentGitStatusAction(), map[string]any{})
	stdout, _ := out["stdout"].(string)
	if !strings.Contains(stdout, "??") {
		t.Fatalf("stdout = %q, want untracked status output", stdout)
	}
	if strings.Contains(stdout, "truncated") {
		t.Fatalf("small status should carry no truncation notice: %q", stdout)
	}
}

// files.read: a file within the limit is returned exactly, with no trailer,
// no truncated flag, and no next_offset.
func TestEnvironmentFileReadActionSmallFilePassthrough(t *testing.T) {
	root := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", root)
	writeFile(t, root, "small.txt", "hello")

	out := executeEnvironmentAction(t, NewEnvironmentFileReadAction(), map[string]any{
		"path": "small.txt",
	})
	if out["truncated"] != false {
		t.Fatalf("small read should not be truncated: %#v", out)
	}
	if out["content"] != "hello" {
		t.Fatalf("content = %#v, want exact passthrough", out["content"])
	}
	if _, ok := out["next_offset"]; ok {
		t.Fatalf("small read should carry no next_offset: %#v", out)
	}
}

// logs.tail: output above the byte cap keeps the tail (newest lines) behind a
// prepended notice; output within the cap passes through untouched.
func TestEnvironmentLogsTailActionByteCap(t *testing.T) {
	logDir := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_WORKER_LOG_DIR", logDir)
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "line%03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(logDir, "worker.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	out := executeEnvironmentAction(t, NewEnvironmentLogsTailAction(), map[string]any{
		"max_bytes": 50,
	})
	content, _ := out["content"].(string)
	if !strings.HasPrefix(content, "[logs truncated:") {
		t.Fatalf("content should lead with the truncation notice: %q", content)
	}
	if !strings.Contains(content, "line099") || strings.Contains(content, "line000") {
		t.Fatalf("logs.tail should keep the newest lines, not the oldest: %q", content)
	}
}

func TestEnvironmentLogsTailActionSmallLogPassthrough(t *testing.T) {
	logDir := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_WORKER_LOG_DIR", logDir)
	if err := os.WriteFile(filepath.Join(logDir, "worker.log"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := executeEnvironmentAction(t, NewEnvironmentLogsTailAction(), map[string]any{})
	content, _ := out["content"].(string)
	if strings.Contains(content, "truncated") {
		t.Fatalf("small log should carry no truncation notice: %q", content)
	}
	if !strings.Contains(content, "alpha") || !strings.Contains(content, "beta") {
		t.Fatalf("small log should be returned intact: %q", content)
	}
}

// Direct coverage of the head+tail truncator that backs bash and git
// clone/fetch (whose action wiring needs a live credential broker).
func TestHeadTailTruncator(t *testing.T) {
	// Negative: within budget, byte-for-byte passthrough.
	small := newHeadTailTruncator(100, "stdout", "hint")
	_, _ = small.Write([]byte("hello"))
	if got := small.present(); got != "hello" {
		t.Fatalf("small present = %q, want exact passthrough", got)
	}

	// Boundary: exactly at the limit omits nothing.
	exact := newHeadTailTruncator(10, "stdout", "hint")
	_, _ = exact.Write([]byte("abcdefghij"))
	if got := exact.present(); got != "abcdefghij" {
		t.Fatalf("exact-fit present = %q, want no truncation", got)
	}

	// Positive across multiple writes: keeps first + last, drops the middle.
	big := newHeadTailTruncator(10, "stdout", "publish an artifact")
	_, _ = big.Write([]byte("abcdef"))
	_, _ = big.Write([]byte("ghijklmnop"))
	got := big.present()
	if !strings.HasPrefix(got, "[stdout truncated:") {
		t.Fatalf("present should lead with a notice: %q", got)
	}
	if !strings.Contains(got, "abcde") || !strings.Contains(got, "lmnop") {
		t.Fatalf("present should keep head and tail: %q", got)
	}
	if !strings.Contains(got, "publish an artifact") {
		t.Fatalf("present should carry the hint: %q", got)
	}
}

func TestSplitDiffSections(t *testing.T) {
	single := "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\n"
	if got := splitDiffSections(single); len(got) != 1 {
		t.Fatalf("single-file diff split into %d sections", len(got))
	}
	multi := "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/y b/y\n@@ -1 +1 @@\n-c\n+d\n"
	if got := splitDiffSections(multi); len(got) != 2 {
		t.Fatalf("two-file diff split into %d sections", len(got))
	}
	if got := splitDiffSections("not a diff"); got != nil {
		t.Fatalf("non-diff text split into %#v, want nil", got)
	}
}

func TestTruncateByLines(t *testing.T) {
	// Negative: fits, returned unchanged.
	if got := truncateByLines("a\nb\n", 100, "git status", "hint", false); got != "a\nb\n" {
		t.Fatalf("fitting lines = %q, want passthrough", got)
	}
	// Positive: whole lines only, with a count.
	raw := strings.Repeat("xxxx\n", 5)
	got := truncateByLines(raw, 10, "git status", "hint", false)
	if !strings.HasPrefix(got, "[git status truncated: showing 2 of 5 lines") {
		t.Fatalf("line truncation notice = %q", got)
	}
	// Capped: hitting the capture ceiling marks the total as approximate.
	capped := truncateByLines("a\nb\nc\n", 100, "git status", "hint", true)
	if !strings.Contains(capped, "of 3+ lines") {
		t.Fatalf("capped total should be marked approximate: %q", capped)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int]string{512: "512 B", 1536: "1.5 KB", 2 * 1024 * 1024: "2.0 MB"}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", n, got, want)
		}
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

func newTestRepo(t *testing.T) string {
	t.Helper()
	repo := realPath(t, t.TempDir())
	t.Setenv("MOBIUS_RUNTIME_WORKSPACE", repo)
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	return repo
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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

// setSizeForTest overrides one of the package-level size caps for the duration
// of a test and restores it afterward.
func setSizeForTest(t *testing.T, target *int, v int) {
	t.Helper()
	old := *target
	*target = v
	t.Cleanup(func() { *target = old })
}

// TestConfigureEnvironmentGitIdentityForcesBotIdentity locks in the fix for
// commits being misattributed to the base image's default git identity: the
// managed environment must overwrite that default with the Mobius bot identity,
// while a repo-local identity still takes precedence — the guarantee that makes
// force-setting the global identity safe.
func TestConfigureEnvironmentGitIdentityForcesBotIdentity(t *testing.T) {
	// Sandbox global + system git config to temp files so the test never reads
	// or mutates the developer's real ~/.gitconfig.
	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(tmp, "gitconfig-system"))

	ctx := context.Background()

	// Seed the base image default identity a managed environment ships with —
	// the value the old "set only if unset" guard mistook for a real identity.
	if err := runGitConfigGlobal(ctx, "user.name", "Sprite"); err != nil {
		t.Fatal(err)
	}
	if err := runGitConfigGlobal(ctx, "user.email", "noreply@sprites.dev"); err != nil {
		t.Fatal(err)
	}

	if err := configureEnvironmentGitCredentials(ctx, "env_test"); err != nil {
		t.Fatalf("configureEnvironmentGitCredentials: %v", err)
	}

	// The base image default must be overridden by the Mobius bot identity.
	if got := gitGlobalConfig(t, "user.name"); got != environmentGitUserName {
		t.Fatalf("global user.name = %q, want %q", got, environmentGitUserName)
	}
	if got := gitGlobalConfig(t, "user.email"); got != environmentGitUserEmail {
		t.Fatalf("global user.email = %q, want %q", got, environmentGitUserEmail)
	}

	// Precedence guarantee: a repo-local identity still wins over the forced
	// global one, so a repo or user that sets its own identity keeps control of
	// commit attribution.
	repo := realPath(t, t.TempDir())
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.name", "Repo Local")
	runGit(t, repo, "config", "user.email", "local@example.com")
	writeFile(t, repo, "f.txt", "hi")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "local identity wins")
	if got := gitOutput(t, repo, "log", "-1", "--format=%an <%ae>"); got != "Repo Local <local@example.com>" {
		t.Fatalf("commit author = %q, want repo-local identity to win over forced global", got)
	}
}

// gitGlobalConfig reads a value from the (test-sandboxed) global git config.
func gitGlobalConfig(t *testing.T, key string) string {
	t.Helper()
	return gitOutput(t, "", "config", "--global", "--get", key)
}

// gitOutput runs a git command and returns its trimmed combined output.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
