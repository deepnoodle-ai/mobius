package main

import "testing"

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name    string
		ldflags string
		build   string
		want    string
	}{
		{"ldflags release wins", "v0.0.26", "v0.0.99", "v0.0.26"},
		{"falls back to build info when ldflags is dev", "dev", "v0.0.26", "v0.0.26"},
		{"falls back to build info when ldflags empty", "", "v0.0.26", "v0.0.26"},
		{"dev when both unavailable", "dev", "", "dev"},
		{"dev when build info is local checkout", "dev", "(devel)", "dev"},
		{"trims whitespace", "  ", "  v0.0.26 ", "v0.0.26"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.ldflags, tt.build); got != tt.want {
				t.Fatalf("resolveVersion(%q, %q) = %q, want %q", tt.ldflags, tt.build, got, tt.want)
			}
		})
	}
}
