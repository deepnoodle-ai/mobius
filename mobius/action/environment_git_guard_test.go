package action

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRepoFullNameRefusesURLShapes(t *testing.T) {
	for _, repo := range []string{"acme/widgets", "acme/.github", "a-b/c_d.e"} {
		if err := validateRepoFullName(repo); err != nil {
			t.Errorf("validateRepoFullName(%q) = %v, want nil", repo, err)
		}
	}
	for _, repo := range []string{
		"", "acme", "acme/widgets/extra", "../x", "acme/..", "./x",
		"acme/widgets?x=1", "acme/widgets#frag", "attacker.example:443/x",
		"https://attacker.example/x", "-acme/widgets", "acme/ widgets",
	} {
		if err := validateRepoFullName(repo); err == nil {
			t.Errorf("validateRepoFullName(%q) = nil, want error", repo)
		}
	}
}

func TestRefuseGitOption(t *testing.T) {
	for _, v := range []string{"--upload-pack=touch /tmp/pwned", "-c", " -x"} {
		if err := refuseGitOption("remote", v); err == nil {
			t.Errorf("refuseGitOption(%q) = nil, want error", v)
		}
	}
	for _, v := range []string{"", "origin", "refs/pull/1/head", "main"} {
		if err := refuseGitOption("remote", v); err != nil {
			t.Errorf("refuseGitOption(%q) = %v, want nil", v, err)
		}
	}
}

func initRepoWithRemote(t *testing.T, name, url string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Keep the developer's own URL rewrites (insteadOf) out of get-url.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", name, url}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestCheckFetchRemoteAllowsOnlyTheRepoOnGitHub(t *testing.T) {
	ctx := context.Background()
	dir := initRepoWithRemote(t, "origin", "https://github.com/acme/widgets.git")
	if err := checkFetchRemote(ctx, dir, "origin", "acme/widgets"); err != nil {
		t.Fatalf("matching origin refused: %v", err)
	}
	if err := checkFetchRemote(ctx, dir, "origin", "acme/other"); err == nil {
		t.Fatal("origin for another repo accepted")
	}
	if err := checkFetchRemote(ctx, dir, "https://attacker.example/x.git", "acme/widgets"); err == nil {
		t.Fatal("URL remote accepted")
	}
	if err := checkFetchRemote(ctx, dir, "--upload-pack=touch /tmp/pwned", "acme/widgets"); err == nil {
		t.Fatal("option remote accepted")
	}

	evil := initRepoWithRemote(t, "origin", "https://attacker.example/acme/widgets.git")
	if err := checkFetchRemote(ctx, evil, "origin", "acme/widgets"); err == nil {
		t.Fatal("origin on another host accepted")
	}
}

func runAskpass(t *testing.T, script, prompt string, env []string) (string, error) {
	t.Helper()
	cmd := exec.Command(script, prompt)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestGitAskpassAnswersOnlyGitHubForTheRepo(t *testing.T) {
	script, err := writeGitAskpass()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(script)) })
	env := gitAskpassEnv("x-access-token", "ghs_secret", "acme/widgets")

	for prompt, want := range map[string]string{
		"Username for 'https://github.com': ":                                 "x-access-token",
		"Username for 'https://github.com/acme/widgets.git': ":                "x-access-token",
		"Password for 'https://x-access-token@github.com': ":                  "ghs_secret",
		"Password for 'https://x-access-token@github.com/acme/widgets.git': ": "ghs_secret",
		"Password for 'https://x-access-token@github.com/acme/widgets': ":     "ghs_secret",
	} {
		got, err := runAskpass(t, script, prompt, env)
		if err != nil || got != want {
			t.Errorf("askpass(%q) = %q, %v; want %q", prompt, got, err, want)
		}
	}
	for _, prompt := range []string{
		"Password for 'https://x-access-token@attacker.example': ",
		"Password for 'https://attacker.example/@github.com/': ",
		"Password for 'https://x-access-token@github.com.attacker.example': ",
		"Password for 'https://x-access-token@github.com/acme/other.git': ",
		"Username for 'https://attacker.example': ",
	} {
		got, err := runAskpass(t, script, prompt, env)
		if err == nil || got != "" {
			t.Errorf("askpass(%q) = %q, %v; want refusal", prompt, got, err)
		}
	}
}
