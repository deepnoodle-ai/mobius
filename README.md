# Mobius

Mobius is a work coordination platform for mixed teams of humans, systems, and AI agents. The hosted service is at [mobiusops.ai](https://www.mobiusops.ai/); this repository is where you build workers, drive the API, or use the CLI.

## What's here

- **Go** — `go.mod` at the repo root. The primary library lives at [`github.com/deepnoodle-ai/mobius/mobius`](https://pkg.go.dev/github.com/deepnoodle-ai/mobius/mobius).
- **CLI** — the `mobius` command at [`cmd/mobius/`](cmd/mobius/), built on the Go library. Resource subcommands (`mobius workflows list`, `mobius runs get`, …) plus a `worker` subcommand.
- **Stock actions** — a small library of ready-to-use actions (print, fail, json, time, random) at [`github.com/deepnoodle-ai/mobius/action`](https://pkg.go.dev/github.com/deepnoodle-ai/mobius/action).
- **Python client** — packaged as [`deepnoodle-mobius`](https://pypi.org/project/deepnoodle-mobius/) on PyPI.
- **TypeScript client** — packaged as [`@deepnoodle/mobius`](https://www.npmjs.com/package/@deepnoodle/mobius) on npm.
- **OpenAPI spec** — [`openapi.yaml`](openapi.yaml) at the repo root is the source of truth. All three clients are generated from it and share a [cross-language contract test suite](testdata/contract/).

## Concepts

- A **workflow** is a definition; a **run** is one execution of that workflow. The backend owns the workflow engine.
- A **task** is one action invocation on behalf of a run. Workers claim tasks, execute the corresponding registered action locally, and report the result back.
- An **action** is a named unit of work registered with a worker (e.g. `send_email`, `stripe_charge`).

## Installation

```bash
# Go
go get github.com/deepnoodle-ai/mobius/mobius

# Python
pip install deepnoodle-mobius

# TypeScript
npm install @deepnoodle/mobius
```

## Quick start

### Creating a client

```go
package main

import (
    "context"
    "log"

    "github.com/deepnoodle-ai/mobius/mobius"
)

func main() {
    client := mobius.NewClient(
        mobius.WithAPIKey("mbx_your_api_key_here"),
    )

    run, err := client.StartRun(context.Background(),
        "workflow-id",
        map[string]interface{}{
            "email": "user@example.com",
            "count": 42,
        },
        mobius.WithQueue("default"),
        mobius.WithMetadata(map[string]string{"source": "my-app"}),
    )
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("Run %s started\n", run.Id)
}
```

### Building a worker

```go
package main

import (
    "context"
    "log"

    "github.com/deepnoodle-ai/mobius/mobius"
)

func sendEmail(ctx mobius.Context, params map[string]interface{}) (map[string]interface{}, error) {
    _ = params["email"].(string)
    // send email ...
    return map[string]interface{}{"sent": true}, nil
}

func main() {
    client := mobius.NewClient(
        mobius.WithAPIKey("mbx_your_api_key_here"),
    )

    worker := client.NewWorker(mobius.WorkerConfig{
        WorkerID:        "my-worker-1",
        Name:            "email-sender",
        Version:         "1.0.0",
        Queues:          []string{"emails"},
        Concurrency:     5,
        PollWaitSeconds: 20,
    })

    mobius.RegisterAction(worker, "send_email", sendEmail)

    if err := worker.Run(context.Background()); err != nil {
        log.Fatal(err)
    }
}
```

### Running the stock worker

The `mobius` CLI ships with a `worker` subcommand that registers the stock actions (`print`, `fail`, `json`, `time`, `random`) — enough to exercise trivial and test workflows without writing any code.

```bash
go install github.com/deepnoodle-ai/mobius/cmd/mobius@latest

mobius worker \
    --api-key $MOBIUS_API_KEY \
    --namespace default \
    --queues default
```

Every flag also reads from an environment variable (`MOBIUS_API_URL`, `MOBIUS_API_KEY`, `MOBIUS_NAMESPACE`, `MOBIUS_QUEUES`). Run `mobius worker --help` for the full list. The CLI defaults to `https://api.mobiusops.ai`.

To register your own actions, copy [`cmd/mobius/main.go`](cmd/mobius/main.go) into your own module and add `mobius.RegisterAction(worker, "name", fn)` calls next to `registerStockActions`.

### Watching run progress

```go
ctx := context.Background()

events, err := client.WatchRun(ctx, "run-id", 0)
if err != nil {
    log.Fatal(err)
}

for event := range events {
    switch event.Type {
    case mobius.RunEventTypeRunUpdated:
        log.Printf("Run updated: %+v\n", event.Data)
    case mobius.RunEventTypeStepProgress:
        log.Printf("Step progress: %s\n", event.Data["step_name"])
    case mobius.RunEventTypeActionAppended:
        log.Printf("Action: %s\n", event.Data["action"])
    }
}
```

Use `client.WatchOrgRuns(ctx, 0)` to subscribe to every run in the namespace.

## Documentation

- [Go API reference](https://pkg.go.dev/github.com/deepnoodle-ai/mobius/mobius)
- [Mobius docs](https://docs.mobiusops.ai/)
- [Workflow specification](https://docs.mobiusops.ai/workflows)

## Development

```bash
make tools           # install codegen tools
make generate        # regenerate Go / Python / TypeScript clients from openapi.yaml
make generate-check  # verify committed generated files match the spec
make test            # run every language's test suite
```

### Cross-language contract tests

All three clients share canonical JSON fixtures under [`testdata/contract/`](testdata/contract). Each language has a contract test that round-trips every fixture through its own types and asserts the result is unchanged. Parity is guaranteed when `make test` passes.

## Security

See [SECURITY.md](SECURITY.md) for how to report vulnerabilities.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
