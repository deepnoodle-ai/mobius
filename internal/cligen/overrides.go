package main

// Override lets maintainers adjust, rename, or suppress the CLI command the
// generator would emit for a given OpenAPI operation.
//
//   - Skip=true drops the operation entirely. Use this when the command is
//     hand-written elsewhere in the mobius command package.
//   - Group overrides the subcommand group (default: the operation's first
//     OpenAPI tag).
//   - Command overrides the leaf command name (default: derived from the
//     operationId, e.g. `listAutomations` -> `list`, `getAutomation` -> `get`).
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
// Entries are grouped by command group so the file mirrors how a user reads
// `mobius --help`.
var overrides = map[string]Override{
	// --- actions ----------------------------------------------------------
	// `invoke` isn't in the verb list, so the auto-derive keeps the redundant
	// `-action` suffix; strip it.
	"invokeAction": {Command: "invoke"},

	// --- agents -----------------------------------------------------------
	// Drop the redundant `agent` token that the auto-derivation can't strip
	// (the group name already carries it).
	"provisionAgentInbox":       {Command: "provision-inbox"},
	"saveAgentMessagingBinding": {Command: "save-messaging-binding"},
	// Skill/toolkit assignment ops also live in the `agents` group;
	// auto-derivation would collapse both to `list-assignments` and collide.
	// Spell out the resource.
	"listSkillAssignments":   {Command: "list-skill-assignments"},
	"listToolkitAssignments": {Command: "list-toolkit-assignments"},

	// --- skills -----------------------------------------------------------
	// `import` isn't in the verb list, so the auto-derive keeps the
	// redundant `-skill` suffix; strip it.
	"importSkill": {Command: "import"},

	// --- api-keys ---------------------------------------------------------
	// Drop the redundant `key` token; the group name already carries it.
	"createAPIKey": {Command: "create"},
	"listAPIKeys":  {Command: "list"},
	"getAPIKey":    {Command: "get"},
	"revokeAPIKey": {Command: "revoke"},

	// --- environments -----------------------------------------------------
	"listEnvironments":        {Command: "list"},
	"createEnvironment":       {Command: "create"},
	"attachWorkerEnvironment": {Command: "attach-worker"},
	"acquireEnvironment":      {Command: "acquire"},
	"releaseEnvironmentLease": {Command: "release-lease"},
	"getEnvironment":          {Command: "get"},
	"updateEnvironment":       {Command: "update"},
	"destroyEnvironment":      {Command: "destroy"},
	"reconcileEnvironment":    {Command: "reconcile"},
	"execEnvironment":         {Command: "exec"},
	"writeEnvironmentFile":    {Command: "write-file"},
	"startEnvironmentWorker":  {Command: "start-worker"},

	// --- jobs -------------------------------------------------------------
	// The worker socket is a WebSocket transport endpoint, not a normal JSON
	// request/response operation. The hand-written `mobius worker` command is
	// the public CLI entrypoint for this path.
	"openWorkerSocket": {Skip: true},

	// --- projects ---------------------------------------------------------
	"archiveProject": {Command: "archive"},
	"restoreProject": {Command: "restore"},

	// --- runs -------------------------------------------------------------
	// `stream` isn't in the verb list, so the auto-derive keeps the full
	// `stream-run-events` leaf; shorten it.
	"streamRunEvents": {Command: "stream-run"},

	// --- tables -----------------------------------------------------------
	// Row operations use verbs the auto-derivation doesn't recognise
	// (insert/query/search/upsert) or produce an awkward leaf when combined
	// with the `Table` resource word (bulkInsertTableRows → bulk-table-rows).
	// Spell out the leaf names so every command reads as `<verb>-row(s)`.
	"insertTableRow":      {Command: "insert-row"},
	"upsertTableRow":      {Command: "upsert-row"},
	"queryTableRows":      {Command: "query-rows"},
	"searchTableRows":     {Command: "search-rows"},
	"bulkInsertTableRows": {Command: "bulk-insert-rows"},

	// --- webhooks ---------------------------------------------------------
	"pingWebhook": {Command: "ping"},

	// --- worker-sessions --------------------------------------------------
	"listWorkerSessions": {Command: "list"},
}

// groupDescriptions is an opt-in table of subcommand group descriptions,
// keyed by group name (i.e. the kebab-case tag or explicit Override.Group).
//
// Descriptions should be short noun phrases (roughly 4–8 words) that read
// well when listed vertically in `mobius --help`. Prefer consistent
// grammatical shape across entries.
var groupDescriptions = map[string]string{
	"actions":         "Actions available to automations and agents",
	"agents":          "Agents, sessions, and presence",
	"api-keys":        "Project and organization API keys",
	"artifacts":       "Run output artifacts and storage quota",
	"automations":     "Automation definitions, versions, and runs",
	"catalog":         "Available actions and triggerable events",
	"environments":    "Managed execution environments",
	"projects":        "Projects within the organization",
	"runs":            "Automation runs",
	"skills":          "Skill templates that shape agent behavior and tool access",
	"tables":          "Project-scoped tables and rows",
	"toolkits":        "Sets of tools agents can use to take action",
	"webhooks":        "Outgoing webhook subscriptions",
	"worker-sessions": "Registered worker sessions",
}
