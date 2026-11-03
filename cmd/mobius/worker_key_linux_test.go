//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A child running as the worker's user must not read the key from the
// worker's /proc/<pid>/environ, which keeps the environment the worker started
// with even after the variable is removed. Root can still read it.
func TestHideWorkerKeyProtectsProcEnviron(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any process's environ; run as a normal user")
	}
	if os.Getenv("MOBIUS_TEST_WORKER_KEY_CHILD") == "1" {
		hideWorkerKeyFromChildren()
		out, _ := exec.Command("sh", "-c", `cat /proc/$PPID/environ 2>&1 | tr '\0' '\n'`).CombinedOutput()
		fmt.Print(string(out))
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHideWorkerKeyProtectsProcEnviron$")
	cmd.Env = append(os.Environ(), "MOBIUS_TEST_WORKER_KEY_CHILD=1", "MOBIUS_API_KEY=mbx_proc_secret")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "mbx_proc_secret") {
		t.Fatalf("child read the worker key from /proc/$PPID/environ:\n%s", out)
	}
}
