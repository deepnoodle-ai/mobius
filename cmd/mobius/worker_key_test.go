package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// Code a job starts (environment.bash, a coding agent, a git hook) must not
// inherit the worker's API key.
func TestHideWorkerKeyFromChildren(t *testing.T) {
	t.Setenv(workerAPIKeyEnv, "mbx_worker_secret")
	hideWorkerKeyFromChildren()
	out, err := exec.Command("sh", "-c", `printf '%s' "$MOBIUS_API_KEY"`).Output()
	assert.NoError(t, err)
	assert.Equal(t, "", string(out))
}

// git in a job shell gets credentials from the worker over its socket. The
// shell sends only the request and its job lease; the worker adds its own
// credential when it calls the broker.
func TestGitCredentialSocketRoundTrip(t *testing.T) {
	broker := &fakeBroker{cred: &mobius.EnvironmentGitCredential{
		Username: "x-access-token",
		Token:    "ghs_from_socket",
	}}
	server := startTestGitCredentialServer(t, broker)

	info, err := os.Stat(server.dir)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	var out strings.Builder
	err = requestGitCredential(context.Background(), server.Path(), gitCredentialRequest{
		Protocol:   "https",
		Host:       "github.com",
		Path:       "owner/repo.git",
		LeaseToken: "lease_active",
	}, &out)
	assert.NoError(t, err)
	assert.Equal(t, "owner/repo", broker.calledRepo)
	assert.Equal(t, "lease_active", broker.calledLease)
	assert.Equal(t, "env_123", broker.calledEnv)
	assert.True(t, strings.Contains(out.String(), "password=ghs_from_socket\n"))
}

func TestGitCredentialSocketReportsBrokerError(t *testing.T) {
	broker := &fakeBroker{err: errors.New("active job lease is required\nfor Git credentials")}
	server := startTestGitCredentialServer(t, broker)

	var out strings.Builder
	err := requestGitCredential(context.Background(), server.Path(), gitCredentialRequest{
		Host: "github.com",
		Path: "owner/repo",
	}, &out)
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "active job lease is required for Git credentials"))
	assert.Equal(t, "", out.String())
}

// The helper git runs needs no API key: it reads only the socket path and the
// job lease from its environment. Old global git configs still pass
// --environment, which is accepted and ignored.
func TestGitCredentialHelperCommandUsesSocketWithoutAPIKey(t *testing.T) {
	broker := &fakeBroker{cred: &mobius.EnvironmentGitCredential{Token: "ghs_cmd"}}
	server := startTestGitCredentialServer(t, broker)
	unsetEnv(t, workerAPIKeyEnv)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(gitCredentialSocketEnv, server.Path())
	t.Setenv("MOBIUS_JOB_LEASE_TOKEN", "lease_cmd")

	result := newApp().Test(t,
		cli.TestArgs("git-credential-helper", "get", "--environment", "env_ignored"),
		cli.TestStdin("protocol=https\nhost=github.com\npath=owner/repo.git\n\n"),
	)
	if !result.Success() {
		t.Fatalf("helper failed: %v\nstderr:\n%s", result.Err, result.Stderr)
	}
	assert.Equal(t, "lease_cmd", broker.calledLease)
	assert.Equal(t, "env_123", broker.calledEnv)
	assert.True(t, strings.Contains(result.Stdout, "password=ghs_cmd\n"))
}

func startTestGitCredentialServer(t *testing.T, broker gitCredentialBroker) *gitCredentialServer {
	t.Helper()
	server, err := startGitCredentialServer(context.Background(), broker, "env_123", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server
}
