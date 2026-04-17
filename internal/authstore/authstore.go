// Package authstore persists the browser-based CLI credential that
// `mobius auth login` mints, so subsequent CLI invocations can authenticate
// without re-prompting for an API key.
//
// The v1 storage backend is a single JSON file under the user's config
// directory (typically `~/.config/mobius/credentials.json`) written with
// 0600 permissions. We intentionally keep the format flat and versioned so
// future migrations — OS keychain, multi-profile — can layer on without a
// breaking change.
package authstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// credentialFileVersion is written into every file so we can detect and
// migrate older layouts if we change the schema later.
const credentialFileVersion = 1

// Source identifies how the saved credential was obtained. Today only
// browser-based CLI login mints credentials through this store, but we keep
// the field so `mobius auth status` can later distinguish between flows
// without re-reading the token.
type Source string

const (
	SourceBrowserLogin Source = "browser-login"
)

// Credential is the persisted shape of a CLI credential. All optional
// identity fields may be empty when the server does not return them or when
// the login flow did not collect them.
type Credential struct {
	Version      int    `json:"version"`
	Source       Source `json:"source"`
	APIURL       string `json:"api_url"`
	Token        string `json:"token"`
	CredentialID string `json:"credential_id,omitempty"`
	OrgID        string `json:"org_id,omitempty"`
	OrgName      string `json:"org_name,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	UserEmail    string `json:"user_email,omitempty"`
	UserName     string `json:"user_name,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
}

// Path returns the absolute location of the credentials file.
//
// Resolution order, most specific first:
//  1. MOBIUS_CONFIG_DIR — explicit app-specific override; treated as the
//     directory that directly contains credentials.json. Intended for tests
//     and power users who want multiple local profiles.
//  2. XDG_CONFIG_HOME (when set, non-empty, and absolute, per the XDG Base
//     Directory Specification) → $XDG_CONFIG_HOME/mobius/credentials.json.
//  3. Default: <home>/.config/mobius/credentials.json on every platform.
//     We intentionally do NOT use os.UserConfigDir() because on macOS it
//     returns ~/Library/Application Support, which is the wrong neighborhood
//     for a developer CLI — peers like gh, gcloud, and stripe all live under
//     ~/.config, and users expect mobius to sit alongside them.
func Path() (string, error) {
	if dir := os.Getenv("MOBIUS_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "credentials.json"), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "mobius", "credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate user home dir: %w", err)
	}
	return filepath.Join(home, ".config", "mobius", "credentials.json"), nil
}

// Load reads the saved credential. It returns (nil, nil) when no credential
// file exists, so callers can treat "not logged in" as a normal condition.
func Load() (*Credential, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Credential
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the credential atomically with 0600 permissions. The parent
// directory is created with 0700 if it does not already exist.
func Save(c *Credential) error {
	if c == nil {
		return errors.New("authstore: cannot save nil credential")
	}
	if c.Version == 0 {
		c.Version = credentialFileVersion
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credential: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := os.Chmod(tmpName, 0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace credentials file: %w", err)
	}
	return nil
}

// Delete removes any saved credential. A missing file is not an error — the
// intent is "make sure no credential is persisted."
func Delete() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
