package authstore

import (
	"errors"
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

func TestPutProfilePreservesDefaultWhenUpdatingExistingProfile(t *testing.T) {
	t.Setenv("MOBIUS_CONFIG_DIR", t.TempDir())

	if err := PutProfile("prod", Profile{Token: "mbc_old"}, true); err != nil {
		t.Fatalf("put default profile: %v", err)
	}
	if err := PutProfile("prod", Profile{Token: "mbc_new"}, false); err != nil {
		t.Fatalf("update profile: %v", err)
	}

	profile, err := ResolveProfile("")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if profile.Name != "prod" {
		t.Fatalf("default profile = %q, want prod", profile.Name)
	}
	if profile.Token != "mbc_new" {
		t.Fatalf("token = %q, want updated token", profile.Token)
	}
}

func TestLoadUsesNoDefaultSentinel(t *testing.T) {
	t.Setenv("MOBIUS_CONFIG_DIR", t.TempDir())

	if err := PutProfile("scratch", Profile{Token: "mbc_scratch"}, false); err != nil {
		t.Fatalf("put profile: %v", err)
	}
	store, err := LoadStore()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	p := store.Profiles["scratch"]
	p.Default = false
	store.Profiles["scratch"] = p
	if err := SaveStore(store); err != nil {
		t.Fatalf("save store: %v", err)
	}

	if _, err := ResolveProfile(""); !errors.Is(err, ErrNoDefaultProfile) {
		t.Fatalf("ResolveProfile error = %v, want ErrNoDefaultProfile", err)
	}
	profile, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if profile != nil {
		t.Fatalf("Load profile = %#v, want nil", profile)
	}
}
