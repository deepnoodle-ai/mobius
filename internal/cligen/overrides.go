package main

// Override lets maintainers adjust, rename, or suppress the CLI command the
// generator would emit for a given OpenAPI operation.
//
//   - Skip=true drops the operation entirely. Use this when the command is
//     hand-written elsewhere in the mobius command package.
//   - Group overrides the subcommand group (default: the operation's first
//     OpenAPI tag).
//   - Command overrides the leaf command name (default: derived from the
//     operationId, e.g. `listWorkflows` -> `list`, `getWorkflow` -> `get`).
//   - Description overrides the short help string (default: the OpenAPI
//     operation summary).
type Override struct {
	Skip        bool
	Group       string
	Command     string
	Description string
}

// overrides is the generator's override table, keyed by operationId.
//
// Add an entry here when:
//   - You need to hand-write a command (set Skip: true and implement it in
//     the mobius command package).
//   - The auto-derived group, command name, or description is awkward.
//
// Entries are intentionally verbose so the reason for each override stays
// next to the rule itself.
var overrides = map[string]Override{
	// --- Renamed: strip redundant noun suffix inside the "runs" group ---
	"startRun":  {Command: "start"},
	"resumeRun": {Command: "resume"},

	// --- Skipped: hand-written in cmd/mobius ---
	// The browser-based CLI login flow is hand-written in auth.go because it
	// needs to drive the device challenge, open the browser, poll for
	// completion, and persist the returned credential locally — none of which
	// the generic generator can produce. (The RFC 8628 device authorization
	// and token endpoints themselves are intentionally absent from the typed
	// OpenAPI surface — they use form bodies and OAuth error envelopes — so
	// no Skip entry is needed for those.) The CLI-credential management
	// commands (list / revoke) are also hand-written so they render as
	// ergonomic `mobius auth list` / `mobius auth revoke` subcommands that
	// authenticate using the saved credential rather than forcing --api-key.
	"confirmDeviceCode":   {Skip: true},
	"getAuthContext":      {Skip: true},
	"listCLICredentials":  {Skip: true},
	"revokeCLICredential": {Skip: true},

	// --- Skipped: low-value or webhook-style endpoints ---
	// These accept opaque payloads and are intended for external callers
	// (Slack, third-party webhooks), not interactive CLI use.
	"handleSlackCommands": {Skip: true},
	"handleSlackEvents":   {Skip: true},
	"handleSlackInteract": {Skip: true},
	"slackOAuthCallback":  {Skip: true},
}

// groupDescriptions is an opt-in table of subcommand group descriptions,
// keyed by group name (i.e. the kebab-case tag or explicit Override.Group).
//
// Groups without an entry here render with no description — the group name
// alone is expected to be self-explanatory. Add an entry only when a short
// help string genuinely clarifies the group beyond its name.
var groupDescriptions = map[string]string{}
