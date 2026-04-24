// Package authstore persists browser-issued CLI credentials.
//
// Credentials live in one TOML file with multiple named profiles. Profile
// names are just labels; the target project is an explicit field so multiple
// profiles can point at the same project with different credentials.
package authstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Source identifies how the saved credential was obtained.
type Source string

const (
	SourceBrowserLogin Source = "browser-login"
)

// ErrNoDefaultProfile is returned when no profile is marked default and no
// explicit profile name was requested.
var ErrNoDefaultProfile = errors.New("no default profile")

// Profile is one named CLI credential profile.
type Profile struct {
	Name          string `toml:"-"`
	Default       bool   `toml:"default,omitempty"`
	Source        Source `toml:"source,omitempty"`
	APIURL        string `toml:"endpoint,omitempty"`
	Token         string `toml:"token,omitempty"`
	CredentialID  string `toml:"credential_id,omitempty"`
	OrgID         string `toml:"org_id,omitempty"`
	OrgName       string `toml:"org_name,omitempty"`
	ProjectID     string `toml:"project_id,omitempty"`
	ProjectHandle string `toml:"project,omitempty"`
	UserID        string `toml:"user_id,omitempty"`
	UserEmail     string `toml:"user_email,omitempty"`
	UserName      string `toml:"user_name,omitempty"`
	CreatedAt     string `toml:"created_at,omitempty"`
	LastUsedAt    string `toml:"last_used_at,omitempty"`
}

// Credential is kept as a compatibility alias for older single-profile code.
type Credential = Profile

// RequestToken returns the bearer token to present on HTTP requests. Pinned
// profiles store project_id and project explicitly; when the saved token is
// raw, append the project suffix expected by the API.
func (p Profile) RequestToken() string {
	if p.Token == "" || p.ProjectID == "" || p.ProjectHandle == "" {
		return p.Token
	}
	suffix := "." + p.ProjectHandle
	if strings.HasSuffix(p.Token, suffix) {
		return p.Token
	}
	if credentialHasSuffix(p.Token) {
		return p.Token
	}
	return p.Token + suffix
}

func credentialHasSuffix(token string) bool {
	if !strings.HasPrefix(token, "mbx_") && !strings.HasPrefix(token, "mbc_") {
		return false
	}
	dot := strings.LastIndexByte(token, '.')
	return dot >= 0 && dot != len(token)-1
}

// Store is the on-disk credentials file.
type Store struct {
	Profiles map[string]Profile
}

// Path returns the absolute location of the credentials file.
//
// Resolution order:
//  1. MOBIUS_CREDENTIALS_FILE: explicit file path.
//  2. MOBIUS_CONFIG_DIR: explicit directory, primarily for tests.
//  3. ~/.mobius/credentials.
func Path() (string, error) {
	if path := os.Getenv("MOBIUS_CREDENTIALS_FILE"); path != "" {
		if !filepath.IsAbs(path) {
			abs, err := filepath.Abs(path)
			if err != nil {
				return "", fmt.Errorf("resolve credentials file: %w", err)
			}
			return abs, nil
		}
		return path, nil
	}
	if dir := os.Getenv("MOBIUS_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "credentials"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate user home dir: %w", err)
	}
	return filepath.Join(home, ".mobius", "credentials"), nil
}

// PermissionWarning returns a warning message when the credentials file is
// readable or writable by group/other users.
func PermissionWarning() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("%s has permissions %04o; run `chmod 600 %s`", path, info.Mode().Perm(), path), nil
	}
	return "", nil
}

// LoadStore reads all profiles. A missing file returns an empty store.
func LoadStore() (*Store, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Store{Profiles: map[string]Profile{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	profiles := map[string]Profile{}
	if err := toml.Unmarshal(data, &profiles); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &Store{Profiles: profiles}, nil
}

// SaveStore writes all profiles atomically with 0600 file permissions.
func SaveStore(store *Store) error {
	if store == nil {
		store = &Store{}
	}
	if store.Profiles == nil {
		store.Profiles = map[string]Profile{}
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}
	data, err := toml.Marshal(store.Profiles)
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
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

// ResolveProfile returns the explicitly named profile, the default profile
// when name is empty, or a clear error when neither exists.
func ResolveProfile(name string) (*Profile, error) {
	store, err := LoadStore()
	if err != nil {
		return nil, err
	}
	if name != "" {
		p, ok := store.Profiles[name]
		if !ok {
			return nil, fmt.Errorf("profile %q not found. Run `mobius auth login --profile %s`", name, name)
		}
		p.Name = name
		return &p, nil
	}
	var defaults []string
	for n, p := range store.Profiles {
		if p.Default {
			defaults = append(defaults, n)
		}
	}
	sort.Strings(defaults)
	switch len(defaults) {
	case 0:
		return nil, fmt.Errorf("%w. Run `mobius auth login --profile <name>` or `mobius auth use <name>`", ErrNoDefaultProfile)
	case 1:
		p := store.Profiles[defaults[0]]
		p.Name = defaults[0]
		return &p, nil
	default:
		return nil, fmt.Errorf("multiple default profiles: %s", strings.Join(defaults, ", "))
	}
}

// PutProfile inserts or replaces a profile. If makeDefault is true, all other
// profiles are marked non-default.
func PutProfile(name string, profile Profile, makeDefault bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("profile name is required")
	}
	store, err := LoadStore()
	if err != nil {
		return err
	}
	if store.Profiles == nil {
		store.Profiles = map[string]Profile{}
	}
	existing, exists := store.Profiles[name]
	profile.Name = ""
	if makeDefault {
		for n, p := range store.Profiles {
			p.Default = false
			store.Profiles[n] = p
		}
		profile.Default = true
	} else if exists {
		profile.Default = existing.Default
	} else if len(store.Profiles) == 0 {
		profile.Default = true
	}
	store.Profiles[name] = profile
	return SaveStore(store)
}

// SetDefault marks one existing profile as the default.
func SetDefault(name string) error {
	store, err := LoadStore()
	if err != nil {
		return err
	}
	if _, ok := store.Profiles[name]; !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	for n, p := range store.Profiles {
		p.Default = n == name
		store.Profiles[n] = p
	}
	return SaveStore(store)
}

// DeleteProfile removes one profile.
func DeleteProfile(name string) error {
	store, err := LoadStore()
	if err != nil {
		return err
	}
	if _, ok := store.Profiles[name]; !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	delete(store.Profiles, name)
	return SaveStore(store)
}

// TouchProfile updates the local last-used timestamp for a profile.
func TouchProfile(name, when string) error {
	if name == "" || when == "" {
		return nil
	}
	store, err := LoadStore()
	if err != nil {
		return err
	}
	p, ok := store.Profiles[name]
	if !ok {
		return nil
	}
	p.LastUsedAt = when
	store.Profiles[name] = p
	return SaveStore(store)
}

// Save stores a profile under the default name. It exists for tests and older
// callers that only know about one saved credential.
func Save(c *Profile) error {
	if c == nil {
		return errors.New("authstore: cannot save nil profile")
	}
	return PutProfile("default", *c, true)
}

// Load returns the default profile. It returns (nil, nil) when no credentials
// file exists or no default profile has been selected.
func Load() (*Profile, error) {
	p, err := ResolveProfile("")
	if err != nil {
		if errors.Is(err, ErrNoDefaultProfile) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

// Delete removes the credentials file.
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
