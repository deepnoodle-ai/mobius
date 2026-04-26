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
	// --- Renamed: strip redundant noun suffix inside the command group ---
	"startRun":       {Command: "start"},
	"resumeRun":      {Command: "resume"},
	"archiveProject": {Command: "archive"},
	"restoreProject": {Command: "restore"},

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
// Descriptions should be short noun phrases (roughly 4–8 words) that read
// well when listed vertically in `mobius --help`. Prefer consistent
// grammatical shape across entries.
var groupDescriptions = map[string]string{
	"actions":         "Custom HTTP actions called by workflow steps",
	"agents":          "Agents and agent sessions",
	"audit-logs":      "Organization and project audit log entries",
	"channels":        "Chat channels, members, and messages",
	"groups":          "Member groups for routing interactions",
	"interactions":    "Approval, review, and input prompts",
	"jobs":            "Worker runtime — claim, heartbeat, complete",
	"metrics":         "Platform and workflow metrics",
	"projects":        "Projects within the organization",
	"runs":            "Workflow runs",
	"tools":           "Workflows published as callable tools",
	"triggers":        "Event, schedule, and webhook triggers",
	"webhooks":        "Outgoing webhook subscriptions",
	"worker-sessions": "Registered worker sessions",
	"workflows":       "Workflow definitions",
}
