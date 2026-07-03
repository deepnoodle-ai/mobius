package action

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius"
)

const defaultCommandTimeout = 5 * time.Minute

// approxBytesPerToken converts a token budget into the byte limit the truncators
// enforce. Roughly 4 bytes per token is the common rule of thumb; this is only a
// coarse sizing approximation, so exact tokenizer variation across models and
// content does not matter here.
const approxBytesPerToken = 4

// Output caps are sized in tokens because context, not disk, is the scarce
// resource: the models we target have a 200k-token (often smaller) window, and a
// single tool result should consume only a small fraction of it. They are
// package-level variables (not constants) so they can be tuned at runtime and
// overridden in tests via setSizeForTest; values are read at call time, so an
// override takes effect on the next call.
var (
	// absoluteOutputMaxBytes is the hard ceiling on any single tool result at
	// this layer (~32k tokens, ~16% of a 200k-token window). Every byte-output
	// tool clamps to it; each tool's own default and truncation strategy operate
	// below it.
	absoluteOutputMaxBytes = 32_000 * approxBytesPerToken

	// Per-tool defaults, applied when the caller does not request a size. Each is
	// a fraction of absoluteOutputMaxBytes, leaving headroom a caller can opt
	// into up to the absolute cap.
	defaultCommandOutputMaxBytes       = 16_000 * approxBytesPerToken // bash, git status, git diff
	gitCloneFetchCommandOutputMaxBytes = 16_000 * approxBytesPerToken // clone/fetch progress text
	defaultLogsTailMaxBytes            = 16_000 * approxBytesPerToken
	defaultFileReadMaxBytes            = 16_000 * approxBytesPerToken // per read page; larger files paginate

	// capturedOutputCeiling bounds in-memory buffering for structural truncation
	// (git status/diff). It is a MEMORY guard, not a context budget — only the
	// truncated result reaches the model — so it stays well above the
	// presentation caps to keep file/line counts accurate.
	capturedOutputCeiling = 1 << 20 // 1 MiB

	// defaultFileListMaxScan bounds how many entries files.list scans before
	// ranking and cutting (an entry count, not bytes).
	defaultFileListMaxScan = 5000
)

// Per-tool suggestions embedded in truncation notices. Each is phrased as a
// concrete next action the agent can take to recover the omitted content.
const (
	bashOutputHint      = "redirect large output to a file, then read it back in ranges, or publish it as an artifact"
	gitStatusOutputHint = "scope the check to a pathspec (git status -- <path>) in a large working tree"
	gitDiffOutputHint   = "re-run with a pathspec, --stat, or a lower --context to see the rest"
	gitRemoteOutputHint = "retry with a narrower git operation if you need the full output"
)

type environmentContext interface {
	EnvironmentID() string
	MobiusClient() *mobius.Client
	RunID() string
	StepName() string
}

type environmentLeaseContext interface {
	LeaseToken() string
}

type BashInput struct {
	Command        string            `json:"command"`
	Dir            string            `json:"dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty"`
}

type FileReadInput struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
}

type FileWriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

type FileListInput struct {
	Path       string `json:"path,omitempty"`
	Recursive  bool   `json:"recursive,omitempty"`
	MaxEntries int    `json:"max_entries,omitempty"`
}

type FileDeleteInput struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type GitCloneInput struct {
	RepoFullName string `json:"repo_full_name"`
	Dest         string `json:"dest,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Depth        int    `json:"depth,omitempty"`
}

type GitFetchInput struct {
	RepoFullName string `json:"repo_full_name,omitempty"`
	Dir          string `json:"dir,omitempty"`
	Remote       string `json:"remote,omitempty"`
}

type GitStatusInput struct {
	Dir            string `json:"dir,omitempty"`
	MaxOutputBytes int    `json:"max_output_bytes,omitempty"`
}

type GitDiffInput struct {
	Dir            string `json:"dir,omitempty"`
	Staged         bool   `json:"staged,omitempty"`
	Context        int    `json:"context,omitempty"`
	MaxOutputBytes int    `json:"max_output_bytes,omitempty"`
}

type ArtifactPublishInput struct {
	Path string            `json:"path"`
	Name string            `json:"name,omitempty"`
	Mime string            `json:"mime,omitempty"`
	Tags map[string]string `json:"tags,omitempty"`
}

type ArtifactDownloadInput struct {
	ArtifactID string `json:"artifact_id"`
	Dest       string `json:"dest,omitempty"`
	MaxBytes   int64  `json:"max_bytes,omitempty"`
}

type LogsTailInput struct {
	LogName  string `json:"log_name,omitempty"`
	Tail     int    `json:"tail,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

func NewEnvironmentBashAction() mobius.Action {
	return mobius.NewTypedAction("environment.bash", func(ctx mobius.Context, in BashInput) (any, error) {
		if strings.TrimSpace(in.Command) == "" {
			return nil, fmt.Errorf("command is required")
		}
		timeout := defaultCommandTimeout
		if in.TimeoutSeconds > 0 {
			timeout = time.Duration(in.TimeoutSeconds) * time.Second
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		dir, err := workspacePath(in.Dir)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(runCtx, "bash", "-lc", in.Command)
		configureProcessGroup(cmd)
		cmd.Dir = dir
		// The full worker environment — including MOBIUS_API_KEY — is
		// intentionally visible to sandboxed commands: the environment is
		// single-tenant and agent scripts routinely call the mobius CLI. See
		// SECURITY.md; logs.tail redacts credential-shaped tokens on the way
		// back out.
		cmd.Env = os.Environ()
		for key, value := range in.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		limit := resolveOutputMaxBytes(in.MaxOutputBytes, defaultCommandOutputMaxBytes)
		return runCommand(cmd,
			newHeadTailTruncator(limit, "stdout", bashOutputHint),
			newHeadTailTruncator(limit, "stderr", bashOutputHint))
	})
}

func NewEnvironmentFileReadAction() mobius.Action {
	return mobius.NewTypedAction("environment.files.read", func(ctx mobius.Context, in FileReadInput) (any, error) {
		path, err := workspacePath(in.Path)
		if err != nil {
			return nil, err
		}
		limit := resolveOutputMaxBytes(in.MaxBytes, defaultFileReadMaxBytes)
		offset := in.Offset
		if offset < 0 {
			offset = 0
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		if offset > 0 {
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				return nil, err
			}
		}
		data, err := io.ReadAll(io.LimitReader(file, int64(limit+1)))
		if err != nil {
			return nil, err
		}
		truncated := len(data) > limit
		if truncated {
			data = data[:limit]
		}
		nextOffset := offset + int64(len(data))
		if in.Encoding == "base64" {
			// Binary payload: keep the truncation signal out-of-band so the
			// base64 content stays decodable as a single blob.
			out := map[string]any{"path": in.Path, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(data), "truncated": truncated}
			if truncated {
				out["next_offset"] = nextOffset
			}
			return out, nil
		}
		content := string(data)
		if truncated {
			content += fmt.Sprintf(
				"\n\n[file truncated: read %s ending at byte %d; call again with offset=%d to continue]",
				humanBytes(len(data)), nextOffset, nextOffset)
		}
		out := map[string]any{"path": in.Path, "encoding": "text", "content": content, "truncated": truncated}
		if truncated {
			out["next_offset"] = nextOffset
		}
		return out, nil
	})
}

func NewEnvironmentFileWriteAction() mobius.Action {
	return mobius.NewTypedAction("environment.files.write", func(ctx mobius.Context, in FileWriteInput) (any, error) {
		path, err := workspacePath(in.Path)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if in.Append {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		mode := os.FileMode(0o644)
		if in.Mode != "" {
			modeValue, err := strconv.ParseUint(in.Mode, 8, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid mode: %w", err)
			}
			mode = os.FileMode(modeValue)
		}
		file, err := os.OpenFile(path, flag, mode)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		n, err := file.WriteString(in.Content)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": in.Path, "bytes_written": n}, nil
	})
}

func NewEnvironmentFileListAction() mobius.Action {
	return mobius.NewTypedAction("environment.files.list", func(ctx mobius.Context, in FileListInput) (any, error) {
		root, err := workspacePath(in.Path)
		if err != nil {
			return nil, err
		}
		maxEntries := in.MaxEntries
		if maxEntries <= 0 {
			maxEntries = 200
		}
		type scannedEntry struct {
			m   map[string]any
			mod time.Time
		}
		// Collect up to a scan ceiling, then rank and cut. Scanning a bounded
		// superset lets us surface the most recently modified entries and report
		// an accurate total rather than an off-by-one "looks full" guess.
		scanLimit := maxEntries
		if scanLimit < defaultFileListMaxScan {
			scanLimit = defaultFileListMaxScan
		}
		var scanned []scannedEntry
		scanCapped := false
		add := func(path string, info os.FileInfo) {
			if len(scanned) >= scanLimit {
				scanCapped = true
				return
			}
			rel, _ := filepath.Rel(workspaceRoot(), path)
			scanned = append(scanned, scannedEntry{
				m:   map[string]any{"path": filepath.ToSlash(rel), "dir": info.IsDir(), "size": info.Size(), "modified_at": info.ModTime()},
				mod: info.ModTime(),
			})
		}
		if in.Recursive {
			err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if len(scanned) >= scanLimit {
					scanCapped = true
					return filepath.SkipAll
				}
				add(path, info)
				return nil
			})
		} else {
			var items []os.DirEntry
			items, err = os.ReadDir(root)
			for _, item := range items {
				if len(scanned) >= scanLimit {
					scanCapped = true
					break
				}
				info, statErr := item.Info()
				if statErr != nil {
					return nil, statErr
				}
				add(filepath.Join(root, item.Name()), info)
			}
		}
		if err != nil {
			return nil, err
		}
		sort.SliceStable(scanned, func(i, j int) bool { return scanned[i].mod.After(scanned[j].mod) })
		total := len(scanned)

		// Include newest-first entries until we hit either the requested max or
		// the layer's absolute byte budget, whichever binds first — so a large
		// max_entries can't push the serialized listing past the absolute cap.
		// At least one entry is always returned.
		entries := make([]map[string]any, 0)
		used := 0
		byteCapped := false
		for _, e := range scanned {
			if len(entries) >= maxEntries {
				break
			}
			est := estimatedEntryBytes(e.m)
			if len(entries) > 0 && used+est > absoluteOutputMaxBytes {
				byteCapped = true
				break
			}
			entries = append(entries, e.m)
			used += est
		}
		truncated := len(entries) < total
		out := map[string]any{"path": in.Path, "entries": entries, "truncated": truncated}
		if truncated {
			totalLabel := fmt.Sprintf("%d", total)
			if scanCapped {
				totalLabel = fmt.Sprintf("%d+", total)
			}
			reason := "narrow the path or raise max_entries to see more"
			if byteCapped {
				reason = fmt.Sprintf("output hit the ~%s cap; narrow the path to see more", humanBytes(absoluteOutputMaxBytes))
			}
			out["note"] = fmt.Sprintf("showing %d of %s entries (newest first); %s", len(entries), totalLabel, reason)
		}
		return out, nil
	})
}

func NewEnvironmentFileDeleteAction() mobius.Action {
	return mobius.NewTypedAction("environment.files.delete", func(ctx mobius.Context, in FileDeleteInput) (any, error) {
		if strings.TrimSpace(in.Path) == "" {
			return nil, fmt.Errorf("path is required")
		}
		path, err := workspacePath(in.Path)
		if err != nil {
			return nil, err
		}
		if path == workspaceRootAbs() {
			return nil, fmt.Errorf("refusing to delete workspace root")
		}
		if in.Recursive {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
		if err != nil {
			if os.IsNotExist(err) {
				return map[string]any{"path": in.Path, "deleted": false}, nil
			}
			return nil, err
		}
		return map[string]any{"path": in.Path, "deleted": true}, nil
	})
}

func NewEnvironmentGitCloneAction() mobius.Action {
	return mobius.NewTypedAction("environment.git.clone", func(ctx mobius.Context, in GitCloneInput) (any, error) {
		if strings.TrimSpace(in.RepoFullName) == "" {
			return nil, fmt.Errorf("repo_full_name is required")
		}
		dest := in.Dest
		if dest == "" {
			dest = strings.TrimSuffix(filepath.Base(in.RepoFullName), ".git")
		}
		target, err := workspacePath(dest)
		if err != nil {
			return nil, err
		}
		args := []string{"clone"}
		if in.Depth > 0 {
			args = append(args, "--depth", fmt.Sprint(in.Depth))
		}
		if in.Branch != "" {
			args = append(args, "--branch", in.Branch)
		}
		args = append(args, "https://github.com/"+in.RepoFullName+".git", target)
		result, err := runGitWithCredential(ctx, in.RepoFullName, "clone", "", args)
		if err != nil {
			return result, err
		}
		// Wire a persistent, broker-backed credential helper so ordinary git
		// commands run later from the environment (fetch/pull/push via bash)
		// authenticate without a token ever touching disk. Best-effort: the
		// clone already succeeded, so a config failure is surfaced in the result
		// but does not fail the step.
		if ec, ok := ctx.(environmentContext); ok {
			if cfgErr := configureEnvironmentGitCredentials(ctx, ec.EnvironmentID()); cfgErr != nil {
				if m, ok := result.(map[string]any); ok {
					m["git_credential_helper_error"] = cfgErr.Error()
				}
			}
		}
		return result, nil
	})
}

// Bot identity stamped on commits made inside a managed environment, so agent
// commits are attributable to the Mobius bot rather than to the base image's
// default git identity (`Sprite <noreply@sprites.dev>`) or `unknown <root@…>`.
const (
	environmentGitUserName  = "Mobius Agent"
	environmentGitUserEmail = "noreply@mobiusops.ai"
)

// configureEnvironmentGitCredentials writes the global git config that lets
// arbitrary git commands in the environment authenticate through the Mobius
// credential broker. The config holds no secret — only the environment id and
// the path to this binary — because the helper brokers a fresh token on each
// call. Uses the absolute path to the running worker binary so git can find the
// helper regardless of PATH.
func configureEnvironmentGitCredentials(ctx context.Context, environmentID string) error {
	if strings.TrimSpace(environmentID) == "" {
		return fmt.Errorf("environment id is required to configure git credentials")
	}
	bin, err := os.Executable()
	if err != nil || strings.TrimSpace(bin) == "" {
		bin = "mobius"
	}
	helper := "!" + shellQuoteSingle(bin) + " git-credential-helper --environment " + shellQuoteSingle(environmentID)
	// credential.useHttpPath makes git pass the repository path to the helper so
	// it can broker a token scoped to that exact repository.
	if err := runGitConfigGlobal(ctx, "credential.useHttpPath", "true"); err != nil {
		return err
	}
	if err := runGitConfigGlobal(ctx, "credential.helper", helper); err != nil {
		return err
	}
	setEnvironmentGitIdentity(ctx, "user.name", environmentGitUserName)
	setEnvironmentGitIdentity(ctx, "user.email", environmentGitUserEmail)
	return nil
}

func runGitConfigGlobal(ctx context.Context, key, value string) error {
	cmd := exec.CommandContext(ctx, "git", "config", "--global", key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config --global %s: %w: %s", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setEnvironmentGitIdentity force-sets a global git identity value in a
// Mobius-managed environment so agent commits are attributable to the Mobius
// bot. Forcing the GLOBAL identity is deliberate and safe: git resolves the
// commit identity from higher-precedence sources first, so a repo-local
// identity (`git config user.name` inside the repo) or an explicit
// GIT_AUTHOR_*/GIT_COMMITTER_* environment variable still wins. The only global
// identity a managed environment ships is the base image default
// (`Sprite <noreply@sprites.dev>`) — precisely the value we must replace. An
// earlier "set only if unset" guard treated that default as already-configured
// and so never applied the Mobius identity, misattributing commits to Sprite.
// Best-effort: an identity write failure is ignored — the clone already
// succeeded and git simply falls back to whatever identity it can resolve.
func setEnvironmentGitIdentity(ctx context.Context, key, value string) {
	_ = runGitConfigGlobal(ctx, key, value)
}

// shellQuoteSingle single-quotes s for safe embedding in the `!`-prefixed shell
// string git runs for a credential helper.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func NewEnvironmentGitFetchAction() mobius.Action {
	return mobius.NewTypedAction("environment.git.fetch", func(ctx mobius.Context, in GitFetchInput) (any, error) {
		dir, err := workspacePath(in.Dir)
		if err != nil {
			return nil, err
		}
		repo := strings.TrimSpace(in.RepoFullName)
		if repo == "" {
			repo = repoFullNameFromOrigin(dir)
		}
		remote := in.Remote
		if remote == "" {
			remote = "origin"
		}
		return runGitWithCredential(ctx, repo, "fetch", dir, []string{"fetch", remote, "--prune"})
	})
}

func NewEnvironmentGitStatusAction() mobius.Action {
	return mobius.NewTypedAction("environment.git.status", func(ctx mobius.Context, in GitStatusInput) (any, error) {
		dir, err := workspacePath(in.Dir)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "git", "status", "--short", "--branch")
		cmd.Dir = dir
		limit := resolveOutputMaxBytes(in.MaxOutputBytes, defaultCommandOutputMaxBytes)
		return runCommand(cmd,
			newLineTruncator(limit, "git status", gitStatusOutputHint),
			newHeadTailTruncator(limit, "stderr", gitStatusOutputHint))
	})
}

func NewEnvironmentGitDiffAction() mobius.Action {
	return mobius.NewTypedAction("environment.git.diff", func(ctx mobius.Context, in GitDiffInput) (any, error) {
		dir, err := workspacePath(in.Dir)
		if err != nil {
			return nil, err
		}
		args := []string{"diff"}
		if in.Staged {
			args = append(args, "--staged")
		}
		if in.Context > 0 {
			args = append(args, "--unified="+fmt.Sprint(in.Context))
		}
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		limit := resolveOutputMaxBytes(in.MaxOutputBytes, defaultCommandOutputMaxBytes)
		return runCommand(cmd,
			newDiffTruncator(limit, gitDiffOutputHint),
			newHeadTailTruncator(limit, "stderr", gitDiffOutputHint))
	})
}

func NewEnvironmentArtifactPublishAction() mobius.Action {
	return mobius.NewTypedAction("environment.artifact.publish", func(ctx mobius.Context, in ArtifactPublishInput) (any, error) {
		path, err := workspacePath(in.Path)
		if err != nil {
			return nil, err
		}
		ec, ok := ctx.(environmentContext)
		if !ok || ec.MobiusClient() == nil {
			return nil, fmt.Errorf("mobius client is not available in worker context")
		}
		if lc, ok := ctx.(environmentLeaseContext); ok {
			if leaseToken := strings.TrimSpace(lc.LeaseToken()); leaseToken != "" {
				ref, err := ec.MobiusClient().CreateArtifactRefFromFileWithLease(ctx, path, in.Name, in.Mime, leaseToken, in.Tags)
				if err != nil {
					return nil, err
				}
				return ref, nil
			}
		}
		artifact, err := ec.MobiusClient().CreateArtifactFromFile(ctx, path, in.Name, in.Mime, ec.RunID(), ec.StepName(), in.Tags)
		if err != nil {
			return nil, err
		}
		return artifact, nil
	})
}

func NewEnvironmentArtifactDownloadAction() mobius.Action {
	return mobius.NewTypedAction("environment.artifact.download", func(ctx mobius.Context, in ArtifactDownloadInput) (any, error) {
		if strings.TrimSpace(in.ArtifactID) == "" {
			return nil, fmt.Errorf("artifact_id is required")
		}
		dest := in.Dest
		if strings.TrimSpace(dest) == "" {
			dest = in.ArtifactID
		}
		path, err := workspacePath(dest)
		if err != nil {
			return nil, err
		}
		ec, ok := ctx.(environmentContext)
		if !ok || ec.MobiusClient() == nil {
			return nil, fmt.Errorf("mobius client is not available in worker context")
		}
		out, err := ec.MobiusClient().DownloadArtifactToFile(ctx, in.ArtifactID, path, in.MaxBytes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"artifact_id": in.ArtifactID, "path": dest, "bytes_written": out.BytesWritten}, nil
	})
}

func NewEnvironmentLogsTailAction() mobius.Action {
	return mobius.NewTypedAction("environment.logs.tail", func(ctx mobius.Context, in LogsTailInput) (any, error) {
		logName := in.LogName
		if logName == "" {
			logName = "worker"
		}
		tail := in.Tail
		if tail <= 0 {
			tail = 200
		}
		logDir := os.Getenv("MOBIUS_WORKER_LOG_DIR")
		if logDir == "" {
			logDir = filepath.Join(os.TempDir(), "mobius-worker")
		}
		files := map[string]string{
			"worker":    "worker.log",
			"stdout":    "worker.stdout.log",
			"stderr":    "worker.stderr.log",
			"bootstrap": "bootstrap.log",
		}
		file, ok := files[logName]
		if !ok {
			return nil, fmt.Errorf("log_name must be one of bootstrap, stderr, stdout, worker")
		}
		path := filepath.Join(logDir, file)
		content, err := tailFile(path, tail)
		if err != nil {
			return nil, err
		}
		content = redactSecrets(content)
		maxBytes := resolveOutputMaxBytes(in.MaxBytes, defaultLogsTailMaxBytes)
		// Logs are read newest-last, so keep the tail bytes (most recent) and
		// prepend the notice where an outer truncation is least likely to drop it.
		if len(content) > maxBytes {
			omitted := len(content) - maxBytes
			content = content[len(content)-maxBytes:]
			if nl := strings.IndexByte(content, '\n'); nl >= 0 && nl < len(content)-1 {
				content = content[nl+1:]
			}
			content = fmt.Sprintf(
				"[logs truncated: kept last %s, %s of earlier output omitted; request fewer lines or a specific log]\n\n",
				humanBytes(len(content)), humanBytes(omitted)) + content
		}
		return map[string]any{"log_name": logName, "path": path, "tail": tail, "content": content}, nil
	})
}

func EnvironmentActions() []mobius.Action {
	return []mobius.Action{
		NewEnvironmentBashAction(),
		NewEnvironmentFileReadAction(),
		NewEnvironmentFileWriteAction(),
		NewEnvironmentFileListAction(),
		NewEnvironmentFileDeleteAction(),
		NewEnvironmentGitCloneAction(),
		NewEnvironmentGitFetchAction(),
		NewEnvironmentGitStatusAction(),
		NewEnvironmentGitDiffAction(),
		NewEnvironmentArtifactPublishAction(),
		NewEnvironmentArtifactDownloadAction(),
		NewEnvironmentLogsTailAction(),
	}
}

func workspaceRoot() string {
	if root := strings.TrimSpace(os.Getenv("MOBIUS_RUNTIME_WORKSPACE")); root != "" {
		return root
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func workspacePath(path string) (string, error) {
	root := workspaceRootAbs()
	if root == "" {
		return "", fmt.Errorf("workspace root is unavailable")
	}
	var target string
	if strings.TrimSpace(path) == "" {
		target = root
	} else if filepath.IsAbs(path) {
		target = filepath.Clean(path)
	} else {
		target = filepath.Clean(filepath.Join(root, path))
	}
	if err := assertInsideWorkspace(root, target); err != nil {
		return "", err
	}
	// Resolve symlinks to defeat boundary bypass via in-workspace symlinks
	// that point outside the workspace. The target itself may not exist yet
	// (files.write creates it), so resolve the deepest EXISTING ancestor and
	// re-join the not-yet-existing suffix — otherwise writing through a
	// symlinked parent directory that points outside the workspace would pass
	// the lexical check above.
	resolved, err := resolveDeepestExisting(target)
	if err == nil && resolved != "" {
		resolvedRoot := root
		if rr, err := filepath.EvalSymlinks(root); err == nil {
			resolvedRoot = rr
		}
		if err := assertInsideWorkspace(resolvedRoot, resolved); err != nil {
			return "", err
		}
	}
	return target, nil
}

// resolveDeepestExisting resolves symlinks in the longest existing prefix of
// path and re-joins the remaining (not-yet-created) components. Returns "" if
// no component of the path exists.
func resolveDeepestExisting(path string) (string, error) {
	var suffix []string
	p := path
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", nil // nothing on the path exists
		}
		suffix = append(suffix, filepath.Base(p))
		p = parent
	}
}

func assertInsideWorkspace(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path escapes environment workspace")
	}
	return nil
}

func workspaceRootAbs() string {
	root, err := filepath.Abs(workspaceRoot())
	if err != nil {
		return ""
	}
	return filepath.Clean(root)
}

// streamTruncator captures a command's output stream (bounding its own memory)
// and renders the agent-facing string, self-describing any truncation in-band.
// Each tool picks the strategy that fits its output shape; the only shared
// contract is that truncation is announced in the returned text.
type streamTruncator interface {
	io.Writer
	present() string
}

func runCommand(cmd *exec.Cmd, stdout, stderr streamTruncator) (map[string]any, error) {
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, err
		}
	}
	return map[string]any{
		"stdout":    stdout.present(),
		"stderr":    stderr.present(),
		"exit_code": exitCode,
	}, nil
}

func runGitWithCredential(ctx mobius.Context, repoFullName, operation, dir string, args []string) (any, error) {
	ec, ok := ctx.(environmentContext)
	if !ok || ec.MobiusClient() == nil || ec.EnvironmentID() == "" {
		return nil, fmt.Errorf("environment credential broker is not available")
	}
	cred, err := ec.MobiusClient().CreateEnvironmentGitCredential(ctx, ec.EnvironmentID(), mobius.EnvironmentGitCredentialRequest{
		RepoFullName: repoFullName,
		Operation:    operation,
	})
	if err != nil {
		return nil, err
	}
	askpass, err := writeGitAskpass(cred.Username, cred.Token)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(askpass)) }()
	cmd := exec.CommandContext(ctx, "git", args...)
	configureProcessGroup(cmd)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS="+askpass,
		"MOBIUS_GIT_USERNAME="+cred.Username,
		"MOBIUS_GIT_TOKEN="+cred.Token,
	)
	limit := resolveOutputMaxBytes(0, gitCloneFetchCommandOutputMaxBytes)
	out, err := runCommand(cmd,
		newHeadTailTruncator(limit, "stdout", gitRemoteOutputHint),
		newHeadTailTruncator(limit, "stderr", gitRemoteOutputHint))
	if out != nil {
		out["repo_full_name"] = repoFullName
		out["credential_expires_at"] = cred.ExpiresAt
	}
	return out, err
}

func writeGitAskpass(username, token string) (string, error) {
	dir, err := os.MkdirTemp("", "mobius-git-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "askpass.sh")
	script := "#!/bin/sh\ncase \"$1\" in\n*Username*) printf '%s\\n' \"$MOBIUS_GIT_USERNAME\" ;;\n*) printf '%s\\n' \"$MOBIUS_GIT_TOKEN\" ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}

func repoFullNameFromOrigin(dir string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	remote := strings.TrimSpace(string(out))
	remote = strings.TrimPrefix(remote, "https://github.com/")
	remote = strings.TrimPrefix(remote, "git@github.com:")
	remote = strings.TrimSuffix(remote, ".git")
	return remote
}

// tailFileMaxScanBytes bounds how much of the file's end tailFile reads. Logs
// can grow to gigabytes; reading the whole file to take its tail would OOM the
// worker. The presentation layer truncates far below this anyway.
const tailFileMaxScanBytes = 4 << 20

func tailFile(path string, lines int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	offset := info.Size() - tailFileMaxScanBytes
	if offset < 0 {
		offset = 0
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, tailFileMaxScanBytes))
	if err != nil {
		return "", err
	}
	if offset > 0 {
		// Drop the partial first line of a mid-file read.
		if nl := strings.IndexByte(string(data), '\n'); nl >= 0 && nl < len(data)-1 {
			data = data[nl+1:]
		}
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n"), nil
}

var secretRedactionPattern = regexp.MustCompile(`(mbx_|github_pat_|ghp_|gho_|ghu_|ghs_|ghr_)[A-Za-z0-9_]+`)

func redactSecrets(s string) string {
	return secretRedactionPattern.ReplaceAllString(s, "[redacted]")
}

// resolveOutputMaxBytes picks the effective byte cap for a tool result: the
// caller's requested size if given, else the tool default — always clamped to
// absoluteOutputMaxBytes so no single tool result at this layer can exceed it.
func resolveOutputMaxBytes(requested, toolDefault int) int {
	limit := toolDefault
	if limit <= 0 {
		limit = defaultCommandOutputMaxBytes
	}
	if requested > 0 {
		limit = requested
	}
	if limit > absoluteOutputMaxBytes {
		return absoluteOutputMaxBytes
	}
	return limit
}

// headTailTruncator keeps the first and last bytes of a stream, dropping the
// middle. It suits unstructured output (bash, git clone/fetch) where the most
// diagnostic content — the final error — lives at the end. Memory is bounded to
// the presentation limit no matter how much the command emits.
type headTailTruncator struct {
	label   string
	hint    string
	headMax int
	tailMax int
	head    []byte
	tail    []byte
	total   int
}

func newHeadTailTruncator(limit int, label, hint string) *headTailTruncator {
	if limit < 2 {
		limit = 2
	}
	return &headTailTruncator{
		label:   label,
		hint:    hint,
		headMax: limit / 2,
		tailMax: limit - limit/2,
	}
}

func (t *headTailTruncator) Write(p []byte) (int, error) {
	n := len(p)
	t.total += n
	if len(t.head) < t.headMax {
		take := t.headMax - len(t.head)
		if take > len(p) {
			take = len(p)
		}
		t.head = append(t.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		t.tail = append(t.tail, p...)
		if len(t.tail) > t.tailMax {
			t.tail = t.tail[len(t.tail)-t.tailMax:]
		}
	}
	return n, nil
}

func (t *headTailTruncator) present() string {
	if t.total <= t.headMax {
		return string(t.head)
	}
	afterHead := t.total - len(t.head)
	if afterHead <= len(t.tail) {
		// Everything past the head still fit in the tail buffer: nothing lost.
		return string(t.head) + string(t.tail)
	}
	omitted := afterHead - len(t.tail)
	return truncationNotice(t.label, omitted, len(t.head), len(t.tail), t.hint) +
		"\n\n" + string(t.head) +
		fmt.Sprintf("\n\n[... %s omitted ...]\n\n", humanBytes(omitted)) + string(t.tail)
}

// capturedTruncator buffers output up to a memory ceiling and defers truncation
// to a format function that can see structure (whole lines, whole diff files).
// It presents from the head, so the ceiling only needs to exceed what we might
// present — bytes past it are never shown anyway.
type capturedTruncator struct {
	ceiling int
	buf     []byte
	total   int
	format  func(raw string, cappedAtCeiling bool) string
}

func (t *capturedTruncator) Write(p []byte) (int, error) {
	t.total += len(p)
	if len(t.buf) < t.ceiling {
		take := t.ceiling - len(t.buf)
		if take > len(p) {
			take = len(p)
		}
		t.buf = append(t.buf, p[:take]...)
	}
	return len(p), nil
}

func (t *capturedTruncator) present() string {
	return t.format(string(t.buf), t.total > t.ceiling)
}

// newLineTruncator keeps whole lines up to the limit — never cutting mid-line —
// and reports how many lines were shown of the total.
func newLineTruncator(limit int, label, hint string) *capturedTruncator {
	return &capturedTruncator{
		ceiling: capturedOutputCeiling,
		format: func(raw string, cappedAtCeiling bool) string {
			return truncateByLines(raw, limit, label, hint, cappedAtCeiling)
		},
	}
}

// newDiffTruncator keeps whole per-file sections of a unified diff up to the
// limit, so the presented text stays a valid, parseable diff.
func newDiffTruncator(limit int, hint string) *capturedTruncator {
	return &capturedTruncator{
		ceiling: capturedOutputCeiling,
		format: func(raw string, cappedAtCeiling bool) string {
			return truncateDiff(raw, limit, hint, cappedAtCeiling)
		},
	}
}

func truncateByLines(raw string, limit int, label, hint string, cappedAtCeiling bool) string {
	if len(raw) <= limit && !cappedAtCeiling {
		return raw
	}
	lines := strings.SplitAfter(raw, "\n")
	// SplitAfter yields a trailing "" when raw ends in "\n"; drop it.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	var b strings.Builder
	shown := 0
	for _, ln := range lines {
		if shown > 0 && b.Len()+len(ln) > limit {
			break
		}
		b.WriteString(ln)
		shown++
	}
	if shown >= len(lines) && !cappedAtCeiling {
		return raw
	}
	notice := fmt.Sprintf("[%s truncated: showing %d of %s lines", label, shown, countLabel(len(lines), cappedAtCeiling))
	if h := strings.TrimSpace(hint); h != "" {
		notice += " — " + h
	}
	return notice + "]\n\n" + b.String()
}

func truncateDiff(raw string, limit int, hint string, cappedAtCeiling bool) string {
	if len(raw) <= limit && !cappedAtCeiling {
		return raw
	}
	sections := splitDiffSections(raw)
	if len(sections) <= 1 {
		// A single enormous file (or non-standard diff): fall back to head+tail
		// on the bytes so the agent still sees both ends of the change.
		return headTailString(raw, limit, "git diff", hint)
	}
	var b strings.Builder
	shown := 0
	for _, s := range sections {
		if shown > 0 && b.Len()+len(s) > limit {
			break
		}
		b.WriteString(s)
		shown++
	}
	if shown == 0 {
		return headTailString(raw, limit, "git diff", hint)
	}
	if shown >= len(sections) && !cappedAtCeiling {
		return raw
	}
	notice := fmt.Sprintf("[git diff truncated: showing %d of %s files (%s of %s)",
		shown, countLabel(len(sections), cappedAtCeiling), humanBytes(b.Len()), humanBytes(len(raw)))
	if h := strings.TrimSpace(hint); h != "" {
		notice += " — " + h
	}
	return notice + "]\n\n" + b.String()
}

// splitDiffSections splits unified-diff text on "diff --git " boundaries so each
// element is one complete file's diff. Any preamble before the first section is
// dropped (plain `git diff` output has none).
func splitDiffSections(raw string) []string {
	const marker = "diff --git "
	var starts []int
	if strings.HasPrefix(raw, marker) {
		starts = append(starts, 0)
	}
	for i := 0; ; {
		j := strings.Index(raw[i:], "\n"+marker)
		if j < 0 {
			break
		}
		pos := i + j + 1 // index of 'd' in the marker
		starts = append(starts, pos)
		i = pos + len(marker)
	}
	if len(starts) == 0 {
		return nil
	}
	sections := make([]string, 0, len(starts))
	for k, start := range starts {
		end := len(raw)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		sections = append(sections, raw[start:end])
	}
	return sections
}

// headTailString applies head+tail truncation to an already-captured string.
func headTailString(s string, limit int, label, hint string) string {
	if len(s) <= limit {
		return s
	}
	if limit < 2 {
		limit = 2
	}
	head := limit / 2
	tail := limit - head
	omitted := len(s) - head - tail
	return truncationNotice(label, omitted, head, tail, hint) +
		"\n\n" + s[:head] +
		fmt.Sprintf("\n\n[... %s omitted ...]\n\n", humanBytes(omitted)) + s[len(s)-tail:]
}

// truncationNotice renders the leading one-liner for head+tail truncation. It is
// prepended (not appended) so a downstream token cap trims trailing content
// before this signal.
func truncationNotice(label string, omitted, head, tail int, hint string) string {
	notice := fmt.Sprintf("[%s truncated: %s omitted (kept first %s + last %s)",
		label, humanBytes(omitted), humanBytes(head), humanBytes(tail))
	if h := strings.TrimSpace(hint); h != "" {
		notice += " — " + h
	}
	return notice + "]"
}

func countLabel(n int, capped bool) string {
	if capped {
		return fmt.Sprintf("%d+", n)
	}
	return fmt.Sprintf("%d", n)
}

// fileListEntryOverheadBytes approximates the JSON overhead of one files.list
// entry beyond its path: the keys, dir bool, size, and RFC3339 timestamp.
const fileListEntryOverheadBytes = 96

// estimatedEntryBytes approximates the serialized size of one files.list entry,
// dominated by the (variable-length) path. It only needs to be good enough to
// keep the listing under absoluteOutputMaxBytes.
func estimatedEntryBytes(m map[string]any) int {
	p, _ := m["path"].(string)
	return len(p) + fileListEntryOverheadBytes
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
