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
// The recurring rename is the redundant resource token: the auto-derivation
// only strips the resource word for verbs it recognises, so operations like
// `pauseRoutine` or `reviewInteraction` keep a noun the group name already
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
	"previewAgentVisibilityChange": {Command: "preview-visibility-change"},
	"provisionAgentInbox":          {Command: "provision-inbox"},
	"replaceAgentMembers":          {Command: "replace-members"},
	"saveAgentMessagingBinding":    {Command: "save-messaging-binding"},
	"saveAgentMemoryEntry":         {Command: "save-memory-entry"},
	"promoteAgentMemoryEntry":      {Command: "promote-memory-entry"},
	"revertAgentMemoryPromotion":   {Command: "revert-memory-promotion"},
	"replaceAgentSkillAssignments": {Command: "replace-skill-assignments"},
	// Turn-scoped (GET /v1/turns/{turn_id}/messages), not agent-scoped: the
	// bare `list-messages` leaf would read like the session-scoped command of
	// the same name, so keep the turn in the name.
	"listTurnMessages": {Command: "list-turn-messages"},
	// Tagged `sessions` in the spec because it returns a turn transcript, but
	// the path is /v1/agents/invoke and users look for it next to the other
	// agent commands.
	"invokeAgent": {Group: "agents", Command: "invoke"},

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
	"reviewInteraction":    {Command: "review"},

	// --- api-keys ---------------------------------------------------------
	// Drop the redundant `key` token; the group name already carries it.
	"createAPIKey": {Command: "create"},
	"listAPIKeys":  {Command: "list"},
	"getAPIKey":    {Command: "get"},
	"deleteAPIKey": {Command: "delete"},

	// --- organizations ------------------------------------------------------
	// "OAuth" (capital O+A only, not a fully-uppercase initialism like "API")
	// defeats the word-splitting heuristics: the auto-derive lands on
	// `get-auth-return-origins` (drops the "o"). Spell it out lowercase to
	// match the `oauth-return-origins` path segment.
	"getOAuthReturnOrigins": {Command: "get-oauth-return-origins"},
	// Hand-written (organizations.go): the generated body assembly rejects an
	// empty origins list, but an empty list is the documented way to disable
	// embedded return — the hand-written command adds --clear for it.
	"replaceOAuthReturnOrigins": {Skip: true},
	// Pairs with `get-context`, which derives cleanly.
	"replaceOrgContext": {Command: "replace-context"},

	// --- permissions ------------------------------------------------------
	"listOrgPermissions": {Command: "list"},

	// --- blueprints -------------------------------------------------------
	"applyBlueprint":         {Command: "apply"},
	"setBlueprintProtection": {Command: "set-protection"},

	// --- artifacts --------------------------------------------------------
	// Multipart upload: the generated client exposes it only in raw-body
	// form, so `artifacts upload` is hand-written in artifacts.go.
	"createArtifact": {Skip: true},

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

	// --- sessions ---------------------------------------------------------
	// `cancelTurn` and `cancelSession` both auto-derive to `cancel` (the
	// trailing resource word is stripped), so one would land as `cancel-2`.
	// Name the turn-scoped op explicitly so it joins the turn family
	// (`get-turn`/`start-turn`/`list-turns`); `cancelSession` keeps `cancel`.
	"cancelTurn": {Command: "cancel-turn"},
	// Keep nudge lifecycle commands explicit. Without overrides, `cancelNudge`
	// steals the existing `cancel` leaf from `cancelSession`, and `nudgeSession`
	// redundantly renders as `nudge-session` inside the sessions group.
	"nudgeSession":            {Command: "nudge"},
	"cancelNudge":             {Command: "cancel-nudge"},
	"compactSession":          {Command: "compact"},
	"streamSession":           {Command: "stream"},
	"streamSessionTranscript": {Command: "stream-transcript"},
	"appendSessionMessages":   {Command: "append-messages"},
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
}

// groupDescriptions is an opt-in table of subcommand group descriptions,
// keyed by group name (i.e. the kebab-case tag or explicit Override.Group).
//
// Descriptions should be short noun phrases (roughly 4–8 words) that read
// well when listed vertically in `mobius --help`. Prefer consistent
// grammatical shape across entries. Every group the generator emits should
// have one — `mobius --help` prints a bare group name otherwise.
var groupDescriptions = map[string]string{
	"actions":       "Actions that agents and workers invoke",
	"agents":        "Agent identities, memory, and lifecycle",
	"api-keys":      "API keys scoped to the org",
	"artifacts":     "Stored files, uploads, and storage quota",
	"billing":       "Recorded usage events for the org",
	"blueprints":    "Org blueprint application and bindings",
	"catalog":       "Available actions, events, and models",
	"interactions":  "Information, approval, and review requests between users and agents",
	"organizations": "Organization settings and control plane",
	"permissions":   "Assignable org permission catalog",
	"principals":    "Machine identities and their roles",
	"resources":     "Resource custody and audience changes",
	"roles":         "Org roles and assignments",
	"routines":      "Scheduled routines, occurrences, and rosters",
	"sessions":      "Conversation sessions, transcripts, and turns",
	"skills":        "Skill templates that shape agent behavior and tool access",
	"tables":        "Org-scoped tables and rows",
}
