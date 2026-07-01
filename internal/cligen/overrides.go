package main

// Override lets maintainers adjust, rename, or suppress the CLI command the
// generator would emit for a given OpenAPI operation.
//
//   - Skip=true drops the operation entirely. Use this when the command is
//     hand-written elsewhere in the mobius command package.
//   - Group overrides the subcommand group (default: the operation's first
//     OpenAPI tag).
//   - Command overrides the leaf command name (default: derived from the
//     operationId, e.g. `listLoops` -> `list`, `getLoop` -> `get`).
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
	// `put` isn't in the verb list, so the auto-derive keeps the redundant
	// `agent` token; strip it so the leaf matches its siblings
	// `get-memory`/`list-memory-entries`/`delete-memory-entry`.
	"putAgentMemoryEntry": {Command: "put-memory-entry"},
	// `replaceAgentSkillAssignments`/`replaceAgentToolkitAssignments` keep a
	// redundant `agent` token the auto-derivation can't strip (the group
	// already carries it); the matching list ops derive cleanly to
	// `list-skill-assignments`/`list-toolkit-assignments`.
	"replaceAgentSkillAssignments":   {Command: "replace-skill-assignments"},
	"replaceAgentToolkitAssignments": {Command: "replace-toolkit-assignments"},

	// --- skills -----------------------------------------------------------
	// `import` isn't in the verb list, so the auto-derive keeps the
	// redundant `-skill` suffix; strip it.
	"importSkill": {Command: "import"},

	// --- interactions -----------------------------------------------------
	// `respond` isn't in the verb list, so the auto-derive keeps the
	// redundant `-interaction` suffix; strip it (the group already carries it).
	"respondToInteraction": {Command: "respond"},

	// --- api-keys ---------------------------------------------------------
	// Drop the redundant `key` token; the group name already carries it.
	"createAPIKey": {Command: "create"},
	"listAPIKeys":  {Command: "list"},
	"getAPIKey":    {Command: "get"},
	"deleteAPIKey": {Command: "delete"},

	// --- blueprints -------------------------------------------------------
	// Drop the redundant `blueprint` token; the group name already carries it.
	"applyBlueprint": {Command: "apply"},

	// --- environments -----------------------------------------------------
	"listEnvironments":   {Command: "list"},
	"createEnvironment":  {Command: "create"},
	"getEnvironment":     {Command: "get"},
	"updateEnvironment":  {Command: "update"},
	"destroyEnvironment": {Command: "destroy"},

	// --- jobs -------------------------------------------------------------
	// The worker socket is a WebSocket transport endpoint, not a normal JSON
	// request/response operation. The hand-written `mobius worker` command is
	// the public CLI entrypoint for this path.
	"openWorkerSocket": {Skip: true},

	// --- runs -------------------------------------------------------------
	// The group name already says "runs"; keep the leaf names verb-first.
	"signalRun": {Command: "signal"},
	"startRun":  {Command: "start"},

	// --- sessions ---------------------------------------------------------
	// `cancelTurn` and `cancelSession` both auto-derive to `cancel` (the
	// trailing resource word is stripped), so one would land as `cancel-2`.
	// Name the turn-scoped op explicitly so it joins the turn family
	// (`get-turn`/`start-turn`/`list-turns`); `cancelSession` keeps `cancel`.
	"cancelTurn": {Command: "cancel-turn"},
	// `compact` isn't in the verb list, so the auto-derive keeps the
	// redundant `-session` suffix; strip it (the group already carries it).
	"compactSession": {Command: "compact"},
	// Same for `stream` (not a recognised verb): `streamSession` would land as
	// `stream-session`, so strip the redundant suffix to give `sessions stream`.
	"streamSession": {Command: "stream"},

	// --- tables -----------------------------------------------------------
	// Row operations use verbs the auto-derivation doesn't recognise
	// (query/search/upsert) or produce an awkward leaf when combined with the
	// `Table` resource word (bulkCreateTableRows → bulk-table-rows). Spell out
	// the leaf names so every command reads as `<verb>-row(s)`.
	"upsertTableRow":      {Command: "upsert-row"},
	"queryTableRows":      {Command: "query-rows"},
	"searchTableRows":     {Command: "search-rows"},
	"bulkCreateTableRows": {Command: "bulk-create-rows"},

	// --- webhooks ---------------------------------------------------------
	"pingWebhook": {Command: "ping"},
}

// groupDescriptions is an opt-in table of subcommand group descriptions,
// keyed by group name (i.e. the kebab-case tag or explicit Override.Group).
//
// Descriptions should be short noun phrases (roughly 4–8 words) that read
// well when listed vertically in `mobius --help`. Prefer consistent
// grammatical shape across entries.
var groupDescriptions = map[string]string{
	"actions":      "Actions available to loops and agents",
	"agents":       "Agent identities, presence, and lifecycle",
	"api-keys":     "Project and organization API keys",
	"artifacts":    "Run output artifacts and storage quota",
	"blueprints":   "Project blueprint application and bindings",
	"catalog":      "Available actions and triggerable events",
	"environments": "Managed execution environments",
	"interactions": "Information, approval, and review requests between users and agents",
	"loops":        "Loop definitions, versions, and runs",
	"projects":     "Projects within the organization",
	"runs":         "Loop runs",
	"sessions":     "Conversation sessions, transcripts, and invocation",
	"skills":       "Skill templates that shape agent behavior and tool access",
	"tables":       "Project-scoped tables and rows",
	"toolkits":     "Sets of tools agents can use to take action",
	"webhooks":     "Outgoing webhook subscriptions",
}
