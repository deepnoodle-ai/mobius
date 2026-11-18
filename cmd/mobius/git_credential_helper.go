package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// gitCredentialSocketEnv names the Unix socket a managed worker serves git
// credentials on. The worker sets it in its own environment, so every job
// shell it starts inherits the path.
const gitCredentialSocketEnv = "MOBIUS_GIT_CREDENTIAL_SOCKET"

// gitCredentialSocketTimeout bounds one request on the socket, including the
// broker call the worker makes for it.
const gitCredentialSocketTimeout = 30 * time.Second

// registerGitCredentialHelperCommand attaches `mobius git-credential-helper`, a
// git credential helper (https://git-scm.com/docs/gitcredentials) backed by the
// Mobius environment git-credential broker.
//
// It lets ordinary `git` commands run from inside a managed environment (for
// example via `environment.bash`) authenticate against private GitHub repos
// without any token ever being written to disk. git invokes this helper on
// demand. The helper sends git's request to the running worker over the
// worker's local socket ($MOBIUS_GIT_CREDENTIAL_SOCKET). The worker finds the
// job the calling process belongs to from its parent processes, and brokers a fresh, repository-scoped token with its own API key and
// returns it. The helper prints it and exits. Nothing is persisted, and the
// worker's API key never enters the shell that runs git.
//
// `mobius worker` wires it in at startup in a managed environment, by writing
// a global git config that points credential.helper at this subcommand (see
// action.ConfigureEnvironmentGitCredentials).
func registerGitCredentialHelperCommand(app *cli.App) {
	app.Command("git-credential-helper").
		Description("Git credential helper backed by Mobius environment credentials (internal)").
		Hidden().
		Flags(
			// Global git configs written by older workers still pass
			// --environment. The worker knows its own environment, so the
			// value is ignored.
			cli.String("environment", "").Hidden(),
		).
		Args("operation").
		Run(func(ctx *cli.Context) error {
			// git calls the helper with one of get/store/erase. It only expects
			// output for get; store/erase are advisory and this helper persists
			// nothing, so they are silent no-ops.
			if ctx.Arg(0) != "get" {
				return nil
			}
			socket := strings.TrimSpace(os.Getenv(gitCredentialSocketEnv))
			if socket == "" {
				return fmt.Errorf("no Mobius worker credential socket: run git from a job in a Mobius environment")
			}
			req, err := parseGitCredentialRequest(ctx.Stdin())
			if err != nil {
				return err
			}
			// The worker finds the job from this process's parents and adds
			// that job's lease itself; no lease is in the job's environment.
			req.LeaseToken = ""
			return requestGitCredential(ctx.Context(), socket, req, ctx.Stdout())
		})
}

// gitCredentialRequest is the subset of git's credential key/value input the
// helper acts on, plus the job lease the helper adds.
type gitCredentialRequest struct {
	Protocol   string
	Host       string
	Path       string
	LeaseToken string
}

// parseGitCredentialRequest reads git's credential protocol from r: `key=value`
// lines terminated by a blank line or EOF.
func parseGitCredentialRequest(r io.Reader) (gitCredentialRequest, error) {
	var req gitCredentialRequest
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "protocol":
			req.Protocol = value
		case "host":
			req.Host = value
		case "path":
			req.Path = value
		case "lease_token":
			req.LeaseToken = value
		}
	}
	if err := scanner.Err(); err != nil {
		return gitCredentialRequest{}, err
	}
	return req, nil
}

// writeTo writes req in git's credential format, with the lease as an extra
// key the worker reads.
func (req gitCredentialRequest) writeTo(w io.Writer) error {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"protocol", req.Protocol},
		{"host", req.Host},
		{"path", req.Path},
		{"lease_token", req.LeaseToken},
	} {
		if strings.ContainsAny(kv[1], "\n\x00") {
			return fmt.Errorf("git credential %s contains a newline", kv[0])
		}
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
		}
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// requestGitCredential sends req to the worker's credential socket and copies
// the credential it returns to out. A worker-side failure comes back as one
// `error=` line and is returned as an error, so git reports it and falls back
// to its other credential sources.
func requestGitCredential(ctx context.Context, socket string, req gitCredentialRequest, out io.Writer) error {
	dialer := net.Dialer{Timeout: gitCredentialSocketTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("connect to Mobius worker credential socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(gitCredentialSocketTimeout))
	if err := req.writeTo(conn); err != nil {
		return err
	}
	resp, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("read Mobius worker credential socket: %w", err)
	}
	if msg, ok := bytes.CutPrefix(resp, []byte("error=")); ok {
		return errors.New(strings.TrimSpace(string(msg)))
	}
	_, err = out.Write(resp)
	return err
}

// gitCredentialServer is the worker side of the credential socket. It lives
// in a fresh 0700 directory, so only the worker's own user can connect. Job
// shells run as that user and connect on purpose; what they can get is what
// the broker grants for the lease of the job whose process tree holds the
// caller, never the API key itself.
type gitCredentialServer struct {
	dir      string
	listener net.Listener
	// leaseOf finds the job lease for a calling process, as the action
	// socket does. Tests replace it.
	leaseOf func(conn net.Conn) (string, error)
}

// startGitCredentialServer listens on a new Unix socket and brokers a
// credential for each request with broker. Close stops it and removes the
// socket.
func startGitCredentialServer(ctx context.Context, broker gitCredentialBroker, environmentID string, logger *slog.Logger) (*gitCredentialServer, error) {
	// A short name keeps the path under the Unix socket length limit.
	dir, err := os.MkdirTemp("", "mobius-git-")
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "cred.sock"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s := &gitCredentialServer{dir: dir, listener: listener, leaseOf: jobLeaseOfPeer}
	go s.serve(ctx, broker, environmentID, logger)
	return s, nil
}

// Path is the socket path job shells connect to.
func (s *gitCredentialServer) Path() string {
	return s.listener.Addr().String()
}

// Close stops accepting requests and removes the socket directory.
func (s *gitCredentialServer) Close() {
	_ = s.listener.Close()
	_ = os.RemoveAll(s.dir)
}

func (s *gitCredentialServer) serve(ctx context.Context, broker gitCredentialBroker, environmentID string, logger *slog.Logger) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(gitCredentialSocketTimeout))
			reqCtx, cancel := context.WithTimeout(ctx, gitCredentialSocketTimeout)
			defer cancel()
			req, err := parseGitCredentialRequest(conn)
			// The lease comes from the caller's process tree, never from the
			// request: a process outside every running job gets nothing.
			if err == nil {
				req.LeaseToken, err = s.leaseOf(conn)
			}
			var resp bytes.Buffer
			if err == nil {
				err = runGitCredentialHelper(reqCtx, &resp, broker, environmentID, req)
			}
			if err != nil {
				if logger != nil {
					logger.Warn("git credential request failed", "error", err)
				}
				// One line, so the helper can return it as the error text.
				msg := strings.Join(strings.Fields(err.Error()), " ")
				_, _ = fmt.Fprintf(conn, "error=%s\n", msg)
				return
			}
			_, _ = conn.Write(resp.Bytes())
		}()
	}
}

// gitCredentialBroker is the slice of *mobius.Client the helper needs, so the
// core logic is testable without a live client.
type gitCredentialBroker interface {
	CreateEnvironmentGitCredential(ctx context.Context, environmentID string, req mobius.EnvironmentGitCredentialRequest) (*mobius.EnvironmentGitCredential, error)
}

// runGitCredentialHelper brokers a token for the requested repo and writes it to
// out in git credential format. For a request it can't broker (non-GitHub host
// or unparseable path) it writes nothing and returns nil, so git falls back to
// its other credential sources instead of failing on this helper.
func runGitCredentialHelper(ctx context.Context, out io.Writer, broker gitCredentialBroker, environmentID string, req gitCredentialRequest) error {
	repo, ok := githubRepoFromCredentialRequest(req)
	if !ok {
		return nil
	}
	// Always request push: git's credential protocol does not distinguish a
	// fetch from a push, so the helper asks for the strongest scope and lets the
	// Mobius broker decide. The broker mints a write token only when this
	// repository was opted into the environment's push allowlist, otherwise a
	// read token — so read-only fetch/pull keep working and push is a
	// server-enforced opt-in.
	cred, err := broker.CreateEnvironmentGitCredential(ctx, environmentID, mobius.EnvironmentGitCredentialRequest{
		RepoFullName: repo,
		Operation:    "push",
		LeaseToken:   req.LeaseToken,
	})
	if err != nil {
		return fmt.Errorf("broker git credential for %s: %w", repo, err)
	}
	return writeGitCredential(out, cred)
}

// githubRepoFromCredentialRequest derives an `owner/name` full name from a git
// credential request, or reports false when the request is not an https
// github.com remote with a usable path. Requires credential.useHttpPath=true so
// git includes the repository path.
func githubRepoFromCredentialRequest(req gitCredentialRequest) (string, bool) {
	if !strings.EqualFold(req.Host, "github.com") {
		return "", false
	}
	path := strings.TrimPrefix(strings.TrimSpace(req.Path), "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" || strings.Count(path, "/") != 1 {
		return "", false
	}
	return path, true
}

func writeGitCredential(out io.Writer, cred *mobius.EnvironmentGitCredential) error {
	if cred == nil {
		return nil
	}
	username := cred.Username
	if username == "" {
		username = "x-access-token"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "username=%s\n", username)
	fmt.Fprintf(&b, "password=%s\n", cred.Token)
	if !cred.ExpiresAt.IsZero() {
		// git honors password_expiry_utc to avoid caching a token past its life.
		fmt.Fprintf(&b, "password_expiry_utc=%s\n", strconv.FormatInt(cred.ExpiresAt.Unix(), 10))
	}
	_, err := io.WriteString(out, b.String())
	return err
}
