package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius"
	"github.com/deepnoodle-ai/mobius/mobius/action"
)

// actionSocketEnv names the Unix socket a managed worker serves action
// calls on. The worker sets it in its own environment, so every job shell
// inherits the path.
const actionSocketEnv = "MOBIUS_ACTION_SOCKET"

// actionSocketTimeout bounds one request on the socket. Mobius waits at
// most five minutes for an action; this leaves room for the round trip.
const actionSocketTimeout = 6 * time.Minute

// maxAncestorDepth bounds the walk from a caller up to its job's command.
const maxAncestorDepth = 64

// errNotJobProcess refuses a caller that is not inside a running job's
// process tree: an assistant's leftover process, a daemon that outlived its
// parent, or a shell outside any job.
var errNotJobProcess = errors.New("Only a workflow script step can call actions, from its own processes, while it runs. " +
	"This process is not part of a running script step.")

// actionSocketRequest is one request from `mobius actions` or the Python
// helper.
type actionSocketRequest struct {
	Op     string         `json:"op"`
	Action string         `json:"action"`
	Input  map[string]any `json:"input,omitempty"`
	Key    string         `json:"key,omitempty"`
}

// actionSocketResponse is the worker's answer. OK with Output (run) or
// Description (describe); otherwise Error, a sentence for the author.
type actionSocketResponse struct {
	OK          bool                            `json:"ok"`
	Output      any                             `json:"output,omitempty"`
	Description *mobius.ScriptActionDescription `json:"description,omitempty"`
	Code        string                          `json:"code,omitempty"`
	Error       string                          `json:"error,omitempty"`
}

// actionBroker is the slice of *mobius.Client the socket needs.
type actionBroker interface {
	CreateScriptCall(ctx context.Context, environmentID, leaseToken string, req mobius.ScriptCallRequest) (*mobius.ScriptCallResult, error)
	DescribeScriptAction(ctx context.Context, environmentID, leaseToken, action string) (*mobius.ScriptActionDescription, error)
}

// actionSocketServer is the worker side of the action socket. It lives in a
// fresh 0700 directory, so only the worker's own user can connect. A
// connection is served only for a process inside a running job's process
// tree, and the worker adds that job's lease itself: nothing the caller
// sends names a job.
type actionSocketServer struct {
	dir      string
	listener net.Listener
	broker   actionBroker
	envID    string
	logger   *slog.Logger
	// leaseOf finds the job lease for a calling process. Tests replace it.
	leaseOf func(conn net.Conn) (string, error)
}

func startActionSocketServer(ctx context.Context, broker actionBroker, environmentID string, logger *slog.Logger) (*actionSocketServer, error) {
	dir, err := os.MkdirTemp("", "mobius-act-")
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "act.sock"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s := &actionSocketServer{dir: dir, listener: listener, broker: broker, envID: environmentID, logger: logger, leaseOf: jobLeaseOfPeer}
	go s.serve(ctx)
	return s, nil
}

// Path is the socket path job shells connect to.
func (s *actionSocketServer) Path() string { return s.listener.Addr().String() }

// Close stops accepting requests and removes the socket directory.
func (s *actionSocketServer) Close() {
	_ = s.listener.Close()
	_ = os.RemoveAll(s.dir)
}

func (s *actionSocketServer) serve(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(ctx, conn)
	}
}

func (s *actionSocketServer) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(actionSocketTimeout))
	resp := s.respond(ctx, conn)
	if !resp.OK && s.logger != nil {
		s.logger.Info("action socket request refused", "code", resp.Code, "error", resp.Error)
	}
	raw, _ := json.Marshal(resp)
	_, _ = conn.Write(append(raw, '\n'))
}

func (s *actionSocketServer) respond(ctx context.Context, conn net.Conn) actionSocketResponse {
	// Who is calling is decided by the peer process, never by what it
	// sends. The request is still read first: closing a socket with unread
	// data resets it, and the caller would lose the refusal.
	lease, leaseErr := s.leaseOf(conn)
	line, err := bufio.NewReader(io.LimitReader(conn, 8<<20)).ReadBytes('\n')
	if leaseErr != nil {
		return actionSocketResponse{Code: "not_script_step", Error: leaseErr.Error()}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return actionSocketResponse{Code: "bad_request", Error: "could not read the request"}
	}
	var req actionSocketRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return actionSocketResponse{Code: "bad_request", Error: "the request is not valid JSON"}
	}
	if req.Action == "" {
		return actionSocketResponse{Code: "bad_request", Error: "name an action, for example slack.message.post"}
	}
	reqCtx, cancel := context.WithTimeout(ctx, actionSocketTimeout)
	defer cancel()
	switch req.Op {
	case "describe":
		desc, err := s.broker.DescribeScriptAction(reqCtx, s.envID, lease, req.Action)
		if err != nil {
			return actionSocketResponse{Code: "describe_failed", Error: brokerMessage(err)}
		}
		return actionSocketResponse{OK: true, Description: desc}
	case "run":
		result, err := s.broker.CreateScriptCall(reqCtx, s.envID, lease, mobius.ScriptCallRequest{
			Action: req.Action,
			Input:  req.Input,
			Key:    req.Key,
		})
		if err != nil {
			return actionSocketResponse{Code: "call_failed", Error: brokerMessage(err)}
		}
		if result.Status != "completed" {
			resp := actionSocketResponse{Code: "action_failed", Error: "the call failed"}
			if result.Error != nil {
				resp.Code, resp.Error = result.Error.Code, result.Error.Message
			}
			return resp
		}
		return actionSocketResponse{OK: true, Output: result.Output}
	default:
		return actionSocketResponse{Code: "bad_request", Error: fmt.Sprintf("unknown op %q", req.Op)}
	}
}

// brokerMessage is the sentence an API error gives the author.
func brokerMessage(err error) string {
	var apiErr *mobius.APIError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return apiErr.Message
	}
	return err.Error()
}

// jobLeaseOfPeer finds the lease of the job whose process tree holds the
// peer of conn: it walks parent processes from the peer up to a registered
// job command, stopping at PID 1 or at the worker itself.
func jobLeaseOfPeer(conn net.Conn) (string, error) {
	pid, err := peerPID(conn)
	if err != nil || pid <= 0 {
		return "", errNotJobProcess
	}
	return jobLeaseOfPID(pid, parentPID, os.Getpid())
}

func jobLeaseOfPID(pid int, parent func(int) (int, error), self int) (string, error) {
	for depth := 0; depth < maxAncestorDepth && pid > 1 && pid != self; depth++ {
		if lease, ok := action.JobProcesses.Lease(pid); ok {
			return lease, nil
		}
		next, err := parent(pid)
		if err != nil || next == pid {
			break
		}
		pid = next
	}
	return "", errNotJobProcess
}
