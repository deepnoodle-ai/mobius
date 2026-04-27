package main

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"
)

func TestWorkerHelpUsesWorkersFlag(t *testing.T) {
	result := newApp().Test(t, cli.TestArgs("worker", "--help"))
	if !result.Success() {
		t.Fatalf("worker --help failed: %v\nstdout:\n%s\nstderr:\n%s", result.Err, result.Stdout, result.Stderr)
	}
	output := result.Stdout + result.Stderr
	if !strings.Contains(output, "--workers") {
		t.Fatalf("worker help missing --workers flag:\n%s", output)
	}
	if strings.Contains(output, "--concurrency") {
		t.Fatalf("worker help still contains --concurrency flag:\n%s", output)
	}
}
