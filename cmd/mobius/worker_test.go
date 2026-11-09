package main

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"
)

// TestWorkerHelpAdvertisesConcurrencyAndWorkers confirms both
// scaling knobs are surfaced. `--concurrency` is the default and
// preferred; `--workers` is the advanced multi-presence-row option.
func TestWorkerHelpAdvertisesConcurrencyAndWorkers(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("worker", "--help"))
	if !result.Success() {
		t.Fatalf("worker --help failed: %v\nstdout:\n%s\nstderr:\n%s", result.Err, result.Stdout, result.Stderr)
	}
	output := result.Stdout + result.Stderr
	for _, flag := range []string{"--concurrency", "--workers", "--instance-id"} {
		if !strings.Contains(output, flag) {
			t.Fatalf("worker help missing %s flag:\n%s", flag, output)
		}
	}
}

// --computer-id is the documented flag; --environment-id still works for
// servers and Sprites set up before the rename, but help no longer lists it.
func TestWorkerHelpShowsComputerIDNotEnvironmentID(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("worker", "--help"))
	if !result.Success() {
		t.Fatalf("worker --help failed: %v", result.Err)
	}
	output := result.Stdout + result.Stderr
	if !strings.Contains(output, "--computer-id") {
		t.Fatalf("worker help missing --computer-id:\n%s", output)
	}
	if strings.Contains(output, "--environment-id") {
		t.Fatalf("worker help lists the hidden --environment-id:\n%s", output)
	}
}

func TestResolveComputerID(t *testing.T) {
	cases := []struct {
		name, computer, environment, want string
		wantErr                           bool
	}{
		{name: "neither", want: ""},
		{name: "computer only", computer: "env_a", want: "env_a"},
		{name: "environment only", environment: "env_a", want: "env_a"},
		{name: "both agree", computer: "env_a", environment: "env_a", want: "env_a"},
		{name: "both agree after trimming", computer: " env_a", environment: "env_a ", want: "env_a"},
		{name: "both differ", computer: "env_a", environment: "env_b", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveComputerID(tc.computer, tc.environment)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "--computer-id") {
					t.Fatalf("err = %v, want a conflict naming --computer-id", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The worker refuses to start when the new and old computer ID settings
// disagree, whether they come from flags or environment variables.
func TestWorkerFailsWhenComputerAndEnvironmentIDsDiffer(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{
			name: "env vars",
			env: map[string]string{
				"MOBIUS_WORKER_COMPUTER_ID":    "env_a",
				"MOBIUS_WORKER_ENVIRONMENT_ID": "env_b",
			},
		},
		{
			name: "flags",
			args: []string{"--computer-id", "env_a", "--environment-id", "env_b"},
		},
		{
			name: "new flag against old env var",
			env:  map[string]string{"MOBIUS_WORKER_ENVIRONMENT_ID": "env_b"},
			args: []string{"--computer-id", "env_a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MOBIUS_API_KEY", "mbx_test")
			t.Setenv("MOBIUS_API_URL", "https://api.example.invalid")
			t.Setenv("MOBIUS_WORKER_COMPUTER_ID", "")
			t.Setenv("MOBIUS_WORKER_ENVIRONMENT_ID", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			args := append([]string{"worker"}, tc.args...)
			result := newApp().Test(t, cli.TestArgs(args...))
			if result.Success() || result.Err == nil || !strings.Contains(result.Err.Error(), `--computer-id (MOBIUS_WORKER_COMPUTER_ID) is "env_a"`) {
				t.Fatalf("worker = %v, want a computer ID conflict\nstderr:\n%s", result.Err, result.Stderr)
			}
		})
	}
}
