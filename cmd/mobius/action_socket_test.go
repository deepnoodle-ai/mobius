package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"

	"github.com/deepnoodle-ai/mobius/mobius"
	"github.com/deepnoodle-ai/mobius/mobius/action"
)

type fakeActionBroker struct {
	mu     sync.Mutex
	leases []string
	reqs   []mobius.ScriptCallRequest
	result *mobius.ScriptCallResult
	err    error
}

func (f *fakeActionBroker) CreateScriptCall(_ context.Context, _, lease string, req mobius.ScriptCallRequest) (*mobius.ScriptCallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases = append(f.leases, lease)
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeActionBroker) DescribeScriptAction(_ context.Context, _, lease, name string) (*mobius.ScriptActionDescription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases = append(f.leases, lease)
	return &mobius.ScriptActionDescription{Name: name, Title: "Post Slack Message", Effect: "changes"}, nil
}

func (f *fakeActionBroker) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.leases)
}

// A caller is matched to the job whose command is its nearest registered
// ancestor. The walk stops at PID 1 and at the worker itself.
func TestJobLeaseOfPIDWalksToTheJobsCommand(t *testing.T) {
	parents := map[int]int{500: 400, 400: 300, 300: 1, 900: 1, 700: 42}
	parent := func(pid int) (int, error) {
		next, ok := parents[pid]
		if !ok {
			return 0, errors.New("gone")
		}
		return next, nil
	}
	done := action.JobProcesses.Register(300, "lease_job")
	defer done()
	lease, err := jobLeaseOfPID(500, parent, 42)
	assert.NoError(t, err)
	assert.Equal(t, "lease_job", lease)

	_, err = jobLeaseOfPID(900, parent, 42)
	assert.Equal(t, errNotJobProcess, err)
	// The worker's own children are not jobs unless registered.
	_, err = jobLeaseOfPID(700, parent, 42)
	assert.Equal(t, errNotJobProcess, err)

	done()
	_, err = jobLeaseOfPID(500, parent, 42)
	assert.Equal(t, errNotJobProcess, err)
}

func testSocket(t *testing.T, broker actionBroker, leaseOf func(net.Conn) (string, error)) *actionSocketServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "mact-")
	assert.NoError(t, err)
	listener, err := net.Listen("unix", filepath.Join(dir, "a.sock"))
	assert.NoError(t, err)
	s := &actionSocketServer{dir: dir, listener: listener, broker: broker, envID: "env_1", leaseOf: leaseOf}
	go s.serve(context.Background())
	t.Cleanup(s.Close)
	t.Setenv(actionSocketEnv, s.Path())
	return s
}

// The worker sends a call with the lease it found for the caller, and
// passes the result or the refusal back as the author's sentence.
func TestActionSocketRunsCallsForTheCallersJob(t *testing.T) {
	broker := &fakeActionBroker{result: &mobius.ScriptCallResult{Status: "completed", Output: map[string]any{"timestamp": "1.2"}}}
	testSocket(t, broker, func(net.Conn) (string, error) { return "lease_job", nil })
	resp, err := callActionSocket(context.Background(), actionSocketRequest{
		Op: "run", Action: "slack.message.post", Input: map[string]any{"channel": "C1"}, Key: "k1",
	})
	assert.NoError(t, err)
	assert.Equal(t, "1.2", resp.Output.(map[string]any)["timestamp"])
	assert.Equal(t, []string{"lease_job"}, broker.leases)
	assert.Equal(t, "k1", broker.reqs[0].Key)
	assert.Equal(t, "C1", broker.reqs[0].Input["channel"])

	broker.result = &mobius.ScriptCallResult{Status: "failed", Error: &mobius.ScriptCallError{
		Code: "not_declared", Message: "This step cannot call Create Issue (github.issue.create). Add it to the step's Actions.",
	}}
	_, err = callActionSocket(context.Background(), actionSocketRequest{Op: "run", Action: "github.issue.create"})
	assert.Equal(t, "This step cannot call Create Issue (github.issue.create). Add it to the step's Actions.", err.Error())

	broker.err = &mobius.APIError{Status: 403, Message: "Only a workflow script step can call actions."}
	_, err = callActionSocket(context.Background(), actionSocketRequest{Op: "run", Action: "slack.message.post"})
	assert.Equal(t, "Only a workflow script step can call actions.", err.Error())
}

// A caller outside every job is refused before the worker reads or sends
// anything.
func TestActionSocketRefusesProcessesOutsideAJob(t *testing.T) {
	broker := &fakeActionBroker{}
	testSocket(t, broker, func(net.Conn) (string, error) { return "", errNotJobProcess })
	_, err := callActionSocket(context.Background(), actionSocketRequest{Op: "run", Action: "slack.message.post"})
	assert.Equal(t, errNotJobProcess.Error(), err.Error())
	assert.Equal(t, 0, broker.calls())
}

// End to end with real processes: a Python script started by a job's
// command, and its child, call through the helper and are matched to the
// job. A process outside the job is refused.
func TestActionSocketMatchesTheJobsOwnProcesses(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the process check runs on Linux and macOS")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	broker := &fakeActionBroker{result: &mobius.ScriptCallResult{Status: "completed", Output: map[string]any{"ok": true}}}
	testSocket(t, broker, jobLeaseOfPeer)
	helper, err := os.MkdirTemp("", "mpy-")
	assert.NoError(t, err)
	defer func() { _ = os.RemoveAll(helper) }()
	for _, name := range []string{"__init__.py", "actions.py"} {
		data, err := pyHelper.ReadFile("pyhelper/mobius/" + name)
		assert.NoError(t, err)
		assert.NoError(t, os.MkdirAll(filepath.Join(helper, "mobius"), 0o755))
		assert.NoError(t, os.WriteFile(filepath.Join(helper, "mobius", name), data, 0o644))
	}
	script := `
import json, subprocess, sys
from mobius import actions
first = actions.run("slack.message.post", {"channel": "C1"})
child = subprocess.run([sys.executable, "-c",
    "from mobius import actions; import json; print(json.dumps(actions.run('slack.message.post', {})))"],
    capture_output=True, text=True, check=True)
print(json.dumps({"first": first, "child": json.loads(child.stdout)}))
`
	// The job's command is a shell, as environment.bash runs one.
	cmd := exec.Command("/bin/sh", "-c", `exec "$0" -c "$1"; `, python, script)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+helper)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	assert.NoError(t, err)
	assert.NoError(t, cmd.Start())
	done := action.JobProcesses.Register(cmd.Process.Pid, "lease_job")
	var got map[string]any
	decodeErr := json.NewDecoder(stdout).Decode(&got)
	waitErr := cmd.Wait()
	done()
	assert.NoError(t, decodeErr)
	assert.NoError(t, waitErr)
	assert.Equal(t, true, got["first"].(map[string]any)["ok"])
	assert.Equal(t, true, got["child"].(map[string]any)["ok"])
	assert.Equal(t, []string{"lease_job", "lease_job"}, broker.leases)

	// The same helper from a process outside the job is refused.
	outside := exec.Command(python, "-c", "from mobius import actions\n"+
		"try:\n    actions.run('slack.message.post', {})\nexcept actions.ActionError as e:\n    print(e.code + ': ' + str(e))")
	outside.Env = append(os.Environ(), "PYTHONPATH="+helper)
	outside.Stderr = os.Stderr
	out, err := outside.Output()
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(out), "not_script_step: Only a workflow script step can call actions"), string(out))
	assert.Equal(t, 2, broker.calls())
}

func TestFormatActionDescription(t *testing.T) {
	text := formatActionDescription(&mobius.ScriptActionDescription{
		Name:   "slack.message.post",
		Title:  "Post Slack Message",
		Effect: "changes",
		Inputs: []mobius.ScriptActionInput{
			{Name: "channel", Type: "string", Required: true, Description: "Slack channel ID."},
			{Name: "text", Type: "string"},
		},
		OutputSchema: map[string]any{"properties": map[string]any{"timestamp": map[string]any{"type": "string"}}},
	})
	assert.Contains(t, text, "Post Slack Message (slack.message.post): changes something")
	assert.Contains(t, text, "  channel (string, required): Slack channel ID.")
	assert.Contains(t, text, "  text (string)\n")
	assert.Contains(t, text, "  timestamp (string)")
	assert.Contains(t, text, `mobius actions run slack.message.post --input '{"channel":"…"}'`)
}

func TestPrependPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	assert.Equal(t, "/a"+sep+"/b", prependPath("/a", "/b"+sep+"/a"))
	assert.Equal(t, "/a", prependPath("/a", ""))
}
