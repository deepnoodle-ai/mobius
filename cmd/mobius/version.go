package main

import (
	"runtime/debug"
	"strings"
	"time"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = ""
)

// staleBuildThreshold is how old a build can be before `mobius auth status`
// nudges the user to reinstall. The customer-facing CLI moves fast (the
// SDK CLI, not this repo's internal mobiusd CLI) and the most painful
// failure mode is the help text claiming a subcommand exists that the
// installed binary doesn't ship — a stale install. 30 days strikes a
// balance between "low-noise" and "useful nudge."
const staleBuildThreshold = 30 * 24 * time.Hour

func buildVersion() string {
	if version == "" {
		return "dev"
	}
	return version
}

func cliVersion() string {
	v := buildVersion()
	if v != "dev" {
		return v
	}

	parts := []string{v}
	if commit != "" && commit != "unknown" {
		parts = append(parts, commit)
	}
	if date != "" {
		parts = append(parts, date)
	}
	return strings.Join(parts, " ")
}

// staleBuildAge returns how old this binary is, plus a flag for whether
// the age is reliably known. Released builds carry a date stamped via
// ldflags; `go install`-built dev binaries fall back to the VCS commit
// time recorded in the embedded BuildInfo. If neither is available the
// age cannot be trusted (e.g. a tagged release without a date), and we
// return ok=false so callers stay quiet rather than nag without cause.
func staleBuildAge(now time.Time) (time.Duration, bool) {
	if t, ok := parseBuildTimestamp(date); ok {
		return now.Sub(t), true
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.time" {
				if t, ok := parseBuildTimestamp(s.Value); ok {
					return now.Sub(t), true
				}
			}
		}
	}
	return 0, false
}

func parseBuildTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
