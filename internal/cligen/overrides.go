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
// Entries are grouped by command group so the file mirrors how a user reads
// `mobius --help`.
var overrides = map[string]Override{
	// --- actions ----------------------------------------------------------
	// Catalog-action ops collide with the project-scoped action ops on the
	// auto-derived `get` / `list` leaves, so spell them out.
	"invokeAction":       {Command: "invoke"},
	"getCatalogAction":   {Command: "get-catalog"},
	"listCatalogActions": {Command: "list-catalog"},

	// --- agents -----------------------------------------------------------
	// Drop the redundant `agent` token that the auto-derivation can't strip
	// (the group name already carries it).
	"provisionAgentInbox":        {Command: "provision-inbox"},
	"appendAgentSessionMessages": {Command: "append-session-messages"},

	// --- agent-tools ------------------------------------------------------
	"getAgentToolManifest": {Command: "get-manifest"},

	// --- agents (skill/toolkit assignment ops) ----------------------------
	// These live in the `agents` group; auto-derivation would collapse both
	// to `list-assignments` and collide. Spell out the resource.
	"listSkillAssignments":   {Command: "list-skill-assignments"},
	"listToolkitAssignments": {Command: "list-toolkit-assignments"},

	// --- skills -----------------------------------------------------------
	// `import` isn't in the verb list, so the auto-derive keeps the
	// redundant `-skill` suffix; strip it.
	"importSkill": {Command: "import"},

	// --- artifacts --------------------------------------------------------
	"pinArtifact":    {Command: "pin"},
	"unpinArtifact":  {Command: "unpin"},
	"commitArtifact": {Command: "commit"},
	// The auto-derived leaf is `list-artifacts` (strip `Run`), which reads
	// no differently from `list` and hides the run-scoping. Be explicit.
	"listRunArtifacts": {Command: "list-for-run"},

	// --- api-keys ---------------------------------------------------------
	// Drop the redundant `key` token; the group name already carries it.
	"createAPIKey": {Command: "create"},
	"listAPIKeys":  {Command: "list"},
	"getAPIKey":    {Command: "get"},
	"revokeAPIKey": {Command: "revoke"},

	// --- audit-logs -------------------------------------------------------
	"listAuditLogs":    {Command: "list"},
	"listOrgAuditLogs": {Command: "list-org"},

	// --- channels ---------------------------------------------------------
	// Drop the redundant `channel` token from interaction/entity ops.
	"associateChannelInteraction": {Command: "associate-interaction"},
	"respondToChannelInteraction": {Command: "respond-to-interaction"},
	"shareChannelEntity":          {Command: "share-entity"},
	"releaseChannelInteraction":   {Command: "release-interaction"},

	// --- events -----------------------------------------------------------
	// The `events` group is project integration events; the `Integration`
	// token in the operationId is redundant once you're inside the group.
	"listIntegrationEvents":           {Command: "list"},
	"getIntegrationEvent":             {Command: "get"},
	"listIntegrationEventTestSamples": {Command: "list-test-samples"},
	"createIntegrationEventTestFire":  {Command: "test-fire"},
	"streamProjectEvents":             {Command: "stream-project"},
	"streamRunEvents":                 {Command: "stream-run"},

	// --- environments -----------------------------------------------------
	"listEnvironments":        {Command: "list"},
	"createEnvironment":       {Command: "create"},
	"acquireEnvironment":      {Command: "acquire"},
	"releaseEnvironmentLease": {Command: "release-lease"},
	"getEnvironment":          {Command: "get"},
	"updateEnvironment":       {Command: "update"},
	"destroyEnvironment":      {Command: "destroy"},
	"reconcileEnvironment":    {Command: "reconcile"},
	"execEnvironment":         {Command: "exec"},
	"writeEnvironmentFile":    {Command: "write-file"},
	"startEnvironmentWorker":  {Command: "start-worker"},

	// --- groups -----------------------------------------------------------
	// Differentiate "list groups in project" from "list groups a member is in".
	"listMemberGroups": {Command: "list-for-member"},

	// --- integration-providers -------------------------------------------
	"listIntegrationProviders": {Command: "list"},

	// --- interactions -----------------------------------------------------
	// Drop the redundant `interaction` token; the group name carries it.
	"submitInteractionHandoff":   {Command: "submit-handoff"},
	"acceptInteractionHandoff":   {Command: "accept-handoff"},
	"sendBackInteractionHandoff": {Command: "return-handoff"},
	"castInteractionBallot":      {Command: "cast-ballot"},
	"withdrawInteractionBallot":  {Command: "withdraw-ballot"},
	"closeInteractionVote":       {Command: "close-vote"},
	"releaseInteraction":         {Command: "release"},
	"respondToInteraction":       {Command: "respond"},

	// --- jobs -------------------------------------------------------------
	"reportJob":     {Command: "report"},
	"runJobAction":  {Command: "run-action"},
	"emitJobEvents": {Command: "emit-events"},

	// --- logs -------------------------------------------------------------
	"ingestProjectLogs":     {Command: "ingest"},
	"ingestProjectLogsOTLP": {Command: "ingest-otlp"},
	"listProjectLogs":       {Command: "list"},

	// --- messages ---------------------------------------------------------
	"markMessagesRead": {Command: "mark-read"},

	// --- metrics ----------------------------------------------------------
	"getProjectMetrics": {Command: "get"},

	// --- observables ------------------------------------------------------
	"submitObservableObservation": {Command: "submit-observation"},

	// --- projects ---------------------------------------------------------
	"archiveProject": {Command: "archive"},
	"restoreProject": {Command: "restore"},
	// `deleteProjectConfig` does the same DELETE as the hand-written
	// `projects clear-config` (when called without --key-prefix). Suppress
	// the auto-generated leaf so users see only the ergonomic wrapper.
	"deleteProjectConfig": {Skip: true},

	// --- references -------------------------------------------------------
	"lookupReferences":  {Command: "lookup"},
	"resolveReferences": {Command: "resolve"},

	// --- runs -------------------------------------------------------------
	"startRun":            {Command: "start"},
	"resumeRun":           {Command: "resume"},
	"forkRun":             {Command: "fork"},
	"listRunsForWorkflow": {Command: "list-for-workflow"},
	// startWorkflowRun is the path-bound variant of `runs start`; rename so
	// the relationship to a specific workflow is clear in `mobius runs --help`.
	"startWorkflowRun": {Command: "start-for-workflow"},

	// --- permissions ------------------------------------------------------
	"listProjectPermissions": {Command: "list"},

	// --- roles ------------------------------------------------------------
	// Role-assignment ops live in the same group; spell them out so each
	// command reads as `<verb>` or `<verb>-assignment`.
	"createRoleAssignment": {Command: "create-assignment"},
	"deleteRoleAssignment": {Command: "delete-assignment"},
	"listRoleAssignments":  {Command: "list-assignments"},

	// --- service-accounts -------------------------------------------------
	// Drop the redundant `account` token; the group name already carries it.
	"createServiceAccount": {Command: "create"},
	"listServiceAccounts":  {Command: "list"},
	"getServiceAccount":    {Command: "get"},
	"updateServiceAccount": {Command: "update"},
	"deleteServiceAccount": {Command: "delete"},

	// --- spans ------------------------------------------------------------
	"listProjectSpans":         {Command: "list"},
	"getProjectStepSpanCounts": {Command: "step-counts"},

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

	// --- team -------------------------------------------------------------
	"listProjectTeam": {Command: "list"},

	// --- triggers ---------------------------------------------------------
	"testFireTrigger":         {Command: "test-fire"},
	"deleteAllTriggerTargets": {Command: "delete-targets"},

	// --- user-state -------------------------------------------------------
	"listUserStates":  {Command: "list"},
	"getUserState":    {Command: "get"},
	"upsertUserState": {Command: "upsert"},

	// --- webhooks ---------------------------------------------------------
	"pingWebhook": {Command: "ping"},

	// --- worker-sessions --------------------------------------------------
	"listWorkerSessions": {Command: "list"},

	// --- workflows --------------------------------------------------------
	"validateWorkflowExpressions": {Command: "validate-expressions"},

	// --- Skipped: hand-written in cmd/mobius -----------------------------
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

	// --- Skipped: low-value or webhook-style endpoints -------------------
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
	"actions":               "Custom HTTP actions called by workflow steps",
	"actor-state":           "Reportable actor state and per-target assignments",
	"agent-invocations":     "Agent invocation lifecycle and results",
	"agents":                "Agents, sessions, and presence",
	"agent-tools":           "Resolved agent tool manifests",
	"api-keys":              "Project and organization API keys",
	"artifacts":             "Run output artifacts and storage settings",
	"audit-logs":            "Organization and project audit log entries",
	"channels":              "Chat channels, members, and messages",
	"events":                "Inbound integration events and live SSE streams",
	"environments":          "Managed execution environments",
	"generate":              "LLM message generation",
	"groups":                "Member groups for routing interactions",
	"integration-catalog":   "Available integration providers and capabilities",
	"integration-providers": "Connect and manage third-party integration providers",
	"interactions":          "Approval, review, vote, and handoff prompts",
	"jobs":                  "Worker runtime — claim, heartbeat, complete",
	"logs":                  "Structured log ingestion and retrieval",
	"messages":              "Send, list, and update channel messages",
	"metrics":               "Platform and workflow metrics",
	"observables":           "Tracked observables, observations, and state",
	"permissions":           "Project permission definitions and presets",
	"projects":              "Projects within the organization",
	"references":            "Reference lookup and resolution",
	"roles":                 "Project roles and role assignments",
	"runs":                  "Workflow runs",
	"secrets":               "Project secrets and secret versions",
	"service-accounts":      "Project service accounts for agents and automation",
	"skills":                "Skill templates that shape agent behavior and tool access",
	"spans":                 "Distributed tracing spans and traces",
	"tables":                "Project-scoped tables and rows",
	"team":                  "Project team membership",
	"toolkits":              "Sets of tools agents can use to take action",
	"tools":                 "Workflows published as callable tools",
	"triggers":              "Event, schedule, and webhook triggers",
	"user-state":            "Per-user project state and assignments",
	"webhooks":              "Outgoing webhook subscriptions",
	"worker-sessions":       "Registered worker sessions",
	"workflows":             "Workflow definitions",
}
