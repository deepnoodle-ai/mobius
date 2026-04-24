package authstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfilesRoundTripWithExplicitProjectAndDefault(t *testing.T) {
	t.Setenv("MOBIUS_CONFIG_DIR", t.TempDir())

	err := PutProfile("prod-admin", Profile{
		Source:        SourceBrowserLogin,
		APIURL:        "https://api.example.invalid",
		Token:         "mbc_secret",
		OrgID:         "org_123",
		ProjectID:     "prj_123",
		ProjectHandle: "prod",
	}, true)
	if err != nil {
		t.Fatalf("put profile: %v", err)
	}
	err = PutProfile("prod-viewer", Profile{
		Source:        SourceBrowserLogin,
		APIURL:        "https://api.example.invalid",
		Token:         "mbc_viewer.prod",
		OrgID:         "org_123",
		ProjectID:     "prj_123",
		ProjectHandle: "prod",
	}, false)
	if err != nil {
		t.Fatalf("put second profile: %v", err)
	}

	profile, err := ResolveProfile("")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if profile.Name != "prod-admin" {
		t.Fatalf("default profile = %q, want prod-admin", profile.Name)
	}
	if profile.ProjectHandle != "prod" {
		t.Fatalf("project = %q, want prod", profile.ProjectHandle)
	}
	if got := profile.RequestToken(); got != "mbc_secret.prod" {
		t.Fatalf("request token = %q, want suffixed token", got)
	}

	data, err := os.ReadFile(filepath.Join(os.Getenv("MOBIUS_CONFIG_DIR"), "credentials"))
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "[prod-admin]") || !strings.Contains(text, "project = 'prod'") || !strings.Contains(text, "default = true") {
		t.Fatalf("credentials file missing expected TOML fields:\n%s", text)
	}
}
