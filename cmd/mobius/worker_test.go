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
