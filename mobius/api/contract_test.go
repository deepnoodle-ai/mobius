package api_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// Contract fixtures live at <repo>/internal/testdata/contract and are shared
// with the TypeScript and Python SDKs. Each fixture is round-tripped through
// the corresponding generated type and the result is compared to the original
// fixture as a generic JSON value. Parity with the other SDKs is guaranteed
// when every language's contract test passes against the same fixture set.
//
// If a field is missing from a type or silently dropped, this test fails.
// Do not "fix" the fixture — fix the type (or the spec).

const contractDir = "../../internal/testdata/contract"

type fixtureEntry struct {
	File     string `json:"file"`
	Schema   string `json:"schema"`
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
}

type manifest struct {
	Fixtures []fixtureEntry `json:"fixtures"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(m.Fixtures) == 0 {
		t.Fatal("manifest has no fixtures")
	}
	return m
}

// newForSchema returns a pointer to a zero-valued instance of the Go type
// that maps to the given OpenAPI schema name. Keep this in sync with
// manifest.json. A missing entry fails the test so that adding a fixture
// without a Go binding is caught immediately.
func newForSchema(schema string) (any, bool) {
	switch schema {
	case "JobClaimRequest":
		return &api.JobClaimRequest{}, true
	case "JobClaimDataResponse":
		return &api.JobClaimDataResponse{}, true
	case "JobFenceRequest":
		return &api.JobFenceRequest{}, true
	case "JobHeartbeatDataResponse":
		return &api.JobHeartbeatDataResponse{}, true
	case "JobCompleteRequest":
		return &api.JobCompleteRequest{}, true
	}
	return nil, false
}

func TestContractFixtures(t *testing.T) {
	m := loadManifest(t)

	for _, f := range m.Fixtures {
		f := f
		t.Run(f.File, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(contractDir, f.File))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}

			target, ok := newForSchema(f.Schema)
			if !ok {
				t.Fatalf("no Go binding for schema %q (update newForSchema in contract_test.go)", f.Schema)
			}

			if err := json.Unmarshal(raw, target); err != nil {
				t.Fatalf("unmarshal into %s: %v", f.Schema, err)
			}

			roundTripped, err := json.Marshal(target)
			if err != nil {
				t.Fatalf("marshal %s: %v", f.Schema, err)
			}

			if err := assertJSONEqual(raw, roundTripped); err != nil {
				t.Fatalf("%s round-trip mismatch: %v\noriginal:    %s\nroundtrip:   %s",
					f.Schema, err, raw, roundTripped)
			}
		})
	}
}

// assertJSONEqual decodes both sides to generic values and deep-compares.
// This normalizes away key ordering while still catching missing fields,
// extra fields, and type coercions.
func assertJSONEqual(a, b []byte) error {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return fmt.Errorf("decode a: %w", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return fmt.Errorf("decode b: %w", err)
	}
	if !reflect.DeepEqual(av, bv) {
		return fmt.Errorf("not equal")
	}
	return nil
}
