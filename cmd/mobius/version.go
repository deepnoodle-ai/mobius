package main

import (
	"runtime/debug"
	"strings"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = ""
)

// buildVersion returns the worker/CLI version. Release binaries inject it via
// -ldflags. A `go install github.com/deepnoodle-ai/mobius/cmd/mobius@vX.Y.Z`
// build does NOT run those ldflags, so `version` stays at its "dev" default —
// but the module version is still recorded in the binary's build info, so we
// recover it from there. This matters operationally: Mobius Cloud installs the
// managed worker with `go install @<tag>`, and a worker that reports "dev"
// instead of its real tag makes "which worker actually ran?" impossible to
// answer from worker_sessions.version alone.
func buildVersion() string {
	return resolveVersion(version, buildInfoVersion())
}

// resolveVersion picks the most specific known version: an injected ldflags
// version wins; otherwise the module version from build info; otherwise "dev".
func resolveVersion(ldflagsVersion, buildInfoVersion string) string {
	if v := strings.TrimSpace(ldflagsVersion); v != "" && v != "dev" {
		return v
	}
	if v := strings.TrimSpace(buildInfoVersion); v != "" && v != "(devel)" {
		return v
	}
	return "dev"
}

// buildInfoVersion returns the main module version embedded in the binary by the
// Go toolchain, or "" when it is unavailable or a local-checkout build
// ("(devel)"). Only meaningful for `go install <module>@<version>` builds.
func buildInfoVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	v := strings.TrimSpace(info.Main.Version)
	if v == "" || v == "(devel)" {
		return ""
	}
	return v
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
