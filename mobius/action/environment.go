package action

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius"
)

const defaultCommandTimeout = 5 * time.Minute

type environmentContext interface {
	EnvironmentID() string
	MobiusClient() *mobius.Client
	RunID() string
	StepName() string
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
	Dir string `json:"dir,omitempty"`
}

type GitDiffInput struct {
	Dir     string `json:"dir,omitempty"`
	Staged  bool   `json:"staged,omitempty"`
	Context int    `json:"context,omitempty"`
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
	LogName string `json:"log_name,omitempty"`
	Tail    int    `json:"tail,omitempty"`
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
		cmd.Dir = dir
		cmd.Env = os.Environ()
		for key, value := range in.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		return runCommand(cmd, in.MaxOutputBytes)
	})
}

func NewEnvironmentFileReadAction() mobius.Action {
	return mobius.NewTypedAction("environment.files.read", func(ctx mobius.Context, in FileReadInput) (any, error) {
		path, err := workspacePath(in.Path)
		if err != nil {
			return nil, err
		}
		limit := in.MaxBytes
		if limit <= 0 {
			limit = 512 * 1024
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, int64(limit+1)))
		if err != nil {
			return nil, err
		}
		truncated := len(data) > limit
		if truncated {
			data = data[:limit]
		}
		if in.Encoding == "base64" {
			return map[string]any{"path": in.Path, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(data), "truncated": truncated}, nil
		}
		return map[string]any{"path": in.Path, "encoding": "text", "content": string(data), "truncated": truncated}, nil
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
		defer file.Close()
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
		var entries []map[string]any
		add := func(path string, info os.FileInfo) {
			rel, _ := filepath.Rel(workspaceRoot(), path)
			entries = append(entries, map[string]any{"path": filepath.ToSlash(rel), "dir": info.IsDir(), "size": info.Size(), "modified_at": info.ModTime()})
		}
		if in.Recursive {
			err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if len(entries) >= maxEntries {
					return filepath.SkipAll
				}
				add(path, info)
				return nil
			})
		} else {
			var items []os.DirEntry
			items, err = os.ReadDir(root)
			for _, item := range items {
				if len(entries) >= maxEntries {
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
		return map[string]any{"path": in.Path, "entries": entries, "truncated": len(entries) >= maxEntries}, nil
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
		return runGitWithCredential(ctx, in.RepoFullName, "clone", "", args)
	})
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
		return runCommand(cmd, 256*1024)
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
		return runCommand(cmd, 512*1024)
	})
}

func NewEnvironmentArtifactPublishAction() mobius.Action {
	return mobius.NewTypedAction("environment.artifacts.publish", func(ctx mobius.Context, in ArtifactPublishInput) (any, error) {
		path, err := workspacePath(in.Path)
		if err != nil {
			return nil, err
		}
		ec, ok := ctx.(environmentContext)
		if !ok || ec.MobiusClient() == nil {
			return nil, fmt.Errorf("mobius client is not available in worker context")
		}
		artifact, err := ec.MobiusClient().CreateArtifactFromFile(ctx, path, in.Name, in.Mime, ec.RunID(), ec.StepName(), in.Tags)
		if err != nil {
			return nil, err
		}
		return artifact, nil
	})
}

func NewEnvironmentArtifactDownloadAction() mobius.Action {
	return mobius.NewTypedAction("environment.artifacts.download", func(ctx mobius.Context, in ArtifactDownloadInput) (any, error) {
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
			logName = "stdout"
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
		return map[string]any{"log_name": logName, "path": path, "tail": tail, "content": redactSecrets(content)}, nil
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
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes environment workspace")
	}
	return target, nil
}

func workspaceRootAbs() string {
	root, err := filepath.Abs(workspaceRoot())
	if err != nil {
		return ""
	}
	return filepath.Clean(root)
}

func runCommand(cmd *exec.Cmd, maxBytes int) (map[string]any, error) {
	if maxBytes <= 0 {
		maxBytes = 512 * 1024
	}
	var stdout, stderr limitedBuffer
	stdout.limit = maxBytes
	stderr.limit = maxBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
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
		"stdout":           stdout.String(),
		"stderr":           stderr.String(),
		"exit_code":        exitCode,
		"stdout_truncated": stdout.truncated,
		"stderr_truncated": stderr.truncated,
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
	defer os.RemoveAll(filepath.Dir(askpass))
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS="+askpass,
		"MOBIUS_GIT_USERNAME="+cred.Username,
		"MOBIUS_GIT_TOKEN="+cred.Token,
	)
	out, err := runCommand(cmd, 512*1024)
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
		os.RemoveAll(dir)
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

func tailFile(path string, lines int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n"), nil
}

func redactSecrets(s string) string {
	words := strings.Fields(s)
	for _, word := range words {
		if strings.HasPrefix(word, "mbx_") || strings.HasPrefix(word, "github_pat_") || strings.HasPrefix(word, "ghp_") || strings.HasPrefix(word, "gho_") || strings.HasPrefix(word, "ghu_") || strings.HasPrefix(word, "ghs_") || strings.HasPrefix(word, "ghr_") {
			s = strings.ReplaceAll(s, word, "[redacted]")
		}
	}
	return s
}

type limitedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.Buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.truncated = true
		_, _ = b.Buffer.Write(p[:remaining])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
