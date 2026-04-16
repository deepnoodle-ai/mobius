# mobius-cligen

Code generator that emits `commands.gen.go` (and `commands_<group>.gen.go`) for the `mobius` CLI from the OpenAPI spec and the oapi-codegen client in `api`.

It walks every `*ClientWithResponses` method, matches it to an `operationId` in `openapi.yaml`, applies any overrides from `overrides.go`, and emits one `wonton/cli` command per operation.

Invoked via the `go:generate` directive in `cmd/mobius/app.go` and via `make generate-cli-commands`. Never hand-edit the generated files.

## Customizing generated commands

All customization goes through the `overrides` map in [`overrides.go`](./overrides.go), keyed by OpenAPI `operationId`. Each entry is an `Override`:

| Field | Effect |
|---|---|
| `Skip` | Drop the operation from CLI generation. Use when the command is hand-written in `cmd/mobius`, or when the endpoint is webhook-style and not meant for interactive use. |
| `Group` | Override the subcommand group. Defaults to the operation's first OpenAPI tag. |
| `Command` | Override the leaf command name. Defaults to a name derived from the `operationId` (e.g. `listWorkflows` → `list`, `getWorkflow` → `get`). |
| `Description` | Override the short help text. Defaults to the OpenAPI operation summary. |

Example:

```go
var overrides = map[string]Override{
    // Hidden: webhook endpoint, not for interactive use.
    "handleSlackEvents": {Skip: true},

    // Rename + regroup:
    "listWorkflowRuns": {
        Group:       "runs",
        Command:     "list",
        Description: "List workflow runs in the current namespace",
    },
}
```

## Group descriptions

Group descriptions are opt-in. By default `app.Group("name")` is emitted with no description — the group name alone carries the help. To attach a short help string to a specific group, add an entry to the `groupDescriptions` map in `overrides.go`, keyed by the group name (its kebab-case tag or any explicit `Override.Group`):

```go
var groupDescriptions = map[string]string{
    "workflows": "Manage workflow definitions",
}
```

## Regenerating

After editing `overrides.go`, regenerate:

```bash
make generate-cli-commands
```
