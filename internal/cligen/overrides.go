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
	// --- Skipped: hand-written in cmd/mobius ---
	// (none yet; `worker` is a runtime subcommand with no corresponding API
	// operation, so it does not need to be listed here.)

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
