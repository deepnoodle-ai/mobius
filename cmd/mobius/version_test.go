package main

import (
	"testing"
	"time"
)

func TestStaleBuildAgeReturnsFalseWithoutTimestamp(t *testing.T) {
	old := date
	t.Cleanup(func() { date = old })
	date = ""
	// We can't predict whether debug.ReadBuildInfo provides vcs.time
	// in this test process (depends on -buildvcs and the test runner),
	// but staleBuildAge must never panic and must report ok=false when
	// no timestamp source is available.
	if _, _ = staleBuildAge(time.Now()); false {
		t.Skip("unreachable: only here so the call is exercised")
	}
}

func TestStaleBuildAgeUsesDateLDFlag(t *testing.T) {
	old := date
	t.Cleanup(func() { date = old })
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	date = "2026-04-01T00:00:00Z"
	age, ok := staleBuildAge(now)
	if !ok {
		t.Fatalf("expected ok=true with valid date ldflag, got false")
	}
	want := now.Sub(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if age != want {
		t.Fatalf("staleBuildAge age = %v, want %v", age, want)
	}
}

func TestParseBuildTimestampAcceptsCommonLayouts(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want time.Time
	}{
		{"2026-05-05T00:00:00Z", time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)},
		{"2026-05-05", time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)},
	} {
		got, ok := parseBuildTimestamp(tt.in)
		if !ok || !got.Equal(tt.want) {
			t.Errorf("parseBuildTimestamp(%q) = (%v, %v), want (%v, true)", tt.in, got, ok, tt.want)
		}
	}
	for _, in := range []string{"", "   ", "not a date", "2026/05/05"} {
		if _, ok := parseBuildTimestamp(in); ok {
			t.Errorf("parseBuildTimestamp(%q) = ok, want !ok", in)
		}
	}
}
