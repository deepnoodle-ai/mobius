package main

import "strings"

var (
	version = "dev"
	commit  = "unknown"
	date    = ""
)

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
