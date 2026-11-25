package main

// Override lets maintainers adjust, rename, or suppress the CLI command the
// generator would emit for a given OpenAPI operation.
//
//   - Skip=true drops the operation entirely. Use this when the command is
//     hand-written elsewhere in the mobius command package.
//   - Group overrides the subcommand group (default: the operation's first
//     OpenAPI tag).
//   - Command overrides the leaf command name (default: derived from the
//     operationId, e.g. `listSkills` -> `list`, `getSkill` -> `get`).
//   - Description overrides the short help string (default: the OpenAPI
//     operation summary).
//   - AllowEmptyBody lets the command send an empty body when every body
//     field is optional. By default such a command requires at least one
//     flag or --file, so that a bare `update` does not silently no-op. Set it
//     when the empty body is a real request.
type Override struct {
	Skip           bool
	Group          string
	Command        string
	Description    string
	AllowEmptyBody bool
}

// overrides is the generator's override table, keyed by operationId.
//
// Add an entry here when:
//   - You need to hand-write a command (set Skip: true and implement it in
//     the mobius command package).
//   - The auto-derived group, command name, or description is awkward.
//
// The recurring rename is the redundant resource token: the auto-derivation
// only strips the resource word for verbs it recognises, so operations like
// `pauseRoutine` or `respondToInteraction` keep a noun the group name already
// carries. Strip it so every leaf in a group reads verb-first.
//
// Entries are grouped by command group so the file mirrors how a user reads
// `mobius --help`.
var overrides = map[string]Override{
	// --- actions ----------------------------------------------------------
	"invokeAction": {Command: "invoke"},
	// Hand-written: create and rotate reveal one-time secret material, so the
	// commands require an explicit sink (--secret-file or --show-secret)
	// instead of printing the signing secret by default.
	"createAction":       {Skip: true},
	"rotateActionSecret": {Skip: true},

	// --- agents -----------------------------------------------------------
	"provisionAgentInbox":          {Command: "provision-inbox"},
	"saveAgentMessagingBinding":    {Command: "save-messaging-binding"},
	"saveAgentMemoryEntry":         {Command: "save-memory-entry"},
	"promoteAgentMemoryEntry":      {Command: "promote-memory-entry"},
	"revertAgentMemoryPromotion":   {Command: "revert-memory-promotion"},
	"replaceAgentSkillAssignments": {Command: "replace-skill-assignments"},
	// Lists deleted agents. The default `list-agents` reads as the live list
	// next to `agents list`.
	"listDeletedAgents": {Command: "list-deleted"},
	// `name` is optional: a bare restore keeps the old name.
	"restoreAgent": {AllowEmptyBody: true},

	// --- principals -------------------------------------------------------
	// Hand-written so `principals create NAME --role Operator --with-key`
	// can perform the common role-bearing onboarding sequence in one command.
	"createPrincipal": {Skip: true},

	// --- skills -----------------------------------------------------------
	// Hand-written so `skills import PATH|-` takes the skill document itself
	// (a Claude Code / Dive-style markdown file) instead of a JSON request
	// body wrapping it.
	"importSkill": {Skip: true},

	// --- interactions -----------------------------------------------------
	"respondToInteraction": {Command: "respond"},

	// --- api-keys ---------------------------------------------------------
	// Drop the redundant `key` token; the group name already carries it.
	"createAPIKey": {Command: "create"},
	"listAPIKeys":  {Command: "list"},
	"deleteAPIKey": {Command: "delete"},

	// --- organizations ------------------------------------------------------
	// Pairs with `get-context`, which derives cleanly.
	"replaceOrgContext": {Command: "replace-context"},
	// Pairs with `get-deletion` / `cancel-deletion`, which derive cleanly.
	"scheduleOrgDeletion": {Command: "schedule-deletion"},

	// --- permissions ------------------------------------------------------
	"listOrgPermissions": {Command: "list"},

	// --- blueprints -------------------------------------------------------
	"applyBlueprint":         {Command: "apply"},
	"setBlueprintProtection": {Command: "set-protection"},

	// --- artifacts --------------------------------------------------------
	// Multipart upload: the generated client exposes it only in raw-body
	// form, so `artifacts upload` is hand-written in artifacts.go.
	"createArtifact": {Skip: true},
	// Folders are the directory part of an artifact name, so the folder
	// commands read as their filesystem counterparts. Each is hand-written in
	// artifact_folders.go for a positional argument the generated body/params
	// shape cannot express:
	//   - `artifacts mkdir PATH` instead of `create-folder --path`.
	//   - `artifacts rmdir ID-OR-PATH`, which resolves a path to a folder ID
	//     and explains a `folder_not_empty` refusal.
	"createArtifactFolder": {Skip: true},
	"deleteArtifactFolder": {Skip: true},
	"listArtifactFolders":  {Command: "folders"},

	// --- jobs -------------------------------------------------------------
	// The worker socket is a WebSocket transport endpoint, not a normal JSON
	// request/response operation. The hand-written `mobius worker` command is
	// the public CLI entrypoint for this path.
	"openWorkerSocket": {Skip: true},

	// --- routines ---------------------------------------------------------
	"runRoutineNow":          {Command: "run-now"},
	"pauseRoutine":           {Command: "pause"},
	"resumeRoutine":          {Command: "resume"},
	"approveRoutineProposal": {Command: "approve-proposal"},
	"dismissRoutineProposal": {Command: "dismiss-proposal"},
	"markRoutineRead":        {Command: "mark-read"},

	// --- sessions ---------------------------------------------------------
	// `cancelTurn` and `cancelSession` both auto-derive to `cancel` (the
	// trailing resource word is stripped), so one would land as `cancel-2`.
	// Name the turn-scoped op explicitly so it joins the turn family
	// (`get-turn`/`list-turns`/`delete-turn`); `cancelSession` keeps `cancel`.
	"cancelTurn":      {Command: "cancel-turn"},
	"markSessionRead": {Command: "mark-read"},
	// Without an override, `cancelNudge` steals the existing `cancel` leaf
	// from `cancelSession`. `sendSessionInput` derives to `send-input`.
	"cancelNudge":             {Command: "cancel-nudge"},
	"compactSession":          {Command: "compact"},
	"streamSessionTranscript": {Command: "stream-transcript"},
	// Multipart upload: the generated client exposes it only in raw-body
	// form, so `sessions attach` is hand-written in session_attachments.go.
	"createSessionAttachment": {Skip: true},

	// --- tables -----------------------------------------------------------
	// Row operations use verbs the auto-derivation doesn't recognise
	// (query/search/upsert) or produce an awkward leaf when combined with the
	// `Table` resource word (bulkCreateTableRows → bulk-table-rows). Spell out
	// the leaf names so every command reads as `<verb>-row(s)`.
	"upsertTableRow":      {Command: "upsert-row"},
	"queryTableRows":      {Command: "query-rows"},
	"searchTableRows":     {Command: "search-rows"},
	"bulkCreateTableRows": {Command: "bulk-create-rows"},

	// --- channels and external agents --------------------------------------
	// The spec splits external agent operations across the `channels` and
	// `external-agent` tags, and the derived leaves repeat the resource word
	// (external-agent connect-external-agent). Keep every operation on one
	// external agent under `external-agents`, and the channel operations
	// under `channels` with short verbs.
	"connectExternalAgent":         {Group: "external-agents", Command: "connect"},
	"exchangeExternalAgentToken":   {Group: "external-agents", Command: "token"},
	"getExternalAgent":             {Group: "external-agents", Command: "get"},
	"deleteExternalAgent":          {Group: "external-agents", Command: "delete"},
	"setExternalAgentStatus":       {Group: "external-agents", Command: "set-status"},
	"replaceProjectExternalAgents": {Group: "external-agents", Command: "replace-agents"},
	"replaceChannelExternalAgents": {Command: "replace-external-agents"},
	"findChannelMessages":          {Command: "find-messages"},
	// The generated command reads the whole SSE body before printing, so a
	// live stream never prints. Leave it out until the CLI streams.
	"streamChannels": {Skip: true},
}

// groupDescriptions is an opt-in table of subcommand group descriptions,
// keyed by group name (i.e. the kebab-case tag or explicit Override.Group).
//
// Descriptions should be short noun phrases (roughly 4–8 words) that read
// well when listed vertically in `mobius --help`. Prefer consistent
// grammatical shape across entries. Every group the generator emits should
// have one — `mobius --help` prints a bare group name otherwise.
var groupDescriptions = map[string]string{
	"actions":         "Actions that agents and workers invoke",
	"agents":          "Agent identities, memory, and lifecycle",
	"api-keys":        "API keys scoped to the org",
	"artifacts":       "Stored files, uploads, and storage quota",
	"billing":         "Recorded usage events for the org",
	"blueprints":      "Org blueprint application and bindings",
	"catalog":         "Available actions, events, and models",
	"channels":        "Shared channels, messages, and members",
	"external-agents": "Agents outside Mobius that join channels",
	"interactions":    "Information, approval, and review requests between users and agents",
	"organizations":   "Organization settings and control plane",
	"permissions":     "Assignable org permission catalog",
	"principals":      "Machine identities and their roles",
	"roles":           "Org roles and assignments",
	"routines":        "Scheduled routines, occurrences, and rosters",
	"sessions":        "Conversation sessions, transcripts, and turns",
	"skills":          "Skill templates that shape agent behavior and tool access",
	"tables":          "Org-scoped tables and rows",
}
