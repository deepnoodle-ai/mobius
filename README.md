# Mobius

Mobius is a workflow orchestration platform for humans, systems, and AI agents. This repo contains the `mobius` CLI plus generated SDKs for Go, Python, and TypeScript.

## Installation

```bash
# Homebrew
brew tap deepnoodle-ai/tap
brew install mobius

# Standalone installer (macOS / Linux)
curl -fsSL https://raw.githubusercontent.com/deepnoodle-ai/mobius/main/scripts/install.sh | sh

# Go install
go install github.com/deepnoodle-ai/mobius/cmd/mobius@latest
```

Windows builds are attached to each `v*` GitHub Release as `mobius-windows-amd64.zip`.

## Get Started

Set your API key:

```bash
export MOBIUS_API_KEY=mbx_your_api_key_here
export MOBIUS_PROJECT=default
```

Check the CLI is working:

```bash
mobius --version
mobius --help
```

Inspect the resources available in your project:

```bash
mobius workflows list
mobius runs list
mobius workers list
```

Start the stock worker:

```bash
mobius worker \
    --queues default
```

Run more single-job workers in one process when you need more throughput:

```bash
mobius worker \
    --queues default \
    --workers 5
```

Migration note: older versions of `mobius worker` executed up to 10 jobs in
parallel by default through one worker identity. Workers now execute one active
job lease each; pass `--workers 10` to match the previous default throughput.

The stock worker registers built-in actions like `print`, `fail`, `json`, `time`, and `random`. Run `mobius worker --help` for the full worker flags. Global flags can also be provided via `MOBIUS_API_URL`, `MOBIUS_API_KEY`, `MOBIUS_PROJECT`, and `MOBIUS_LOG_LEVEL`.

## Documentation

- [Go API reference](https://pkg.go.dev/github.com/deepnoodle-ai/mobius/mobius)
- [Mobius docs](https://docs.mobiusops.ai/)
- [Workflow specification](https://docs.mobiusops.ai/workflows)

## SDKs

- Go: `github.com/deepnoodle-ai/mobius/mobius`
- Python: [`deepnoodle-mobius`](https://pypi.org/project/deepnoodle-mobius/)
- TypeScript: [`@deepnoodle/mobius`](https://www.npmjs.com/package/@deepnoodle/mobius)

The SDKs expose two layers:

- A high-level surface for common application and worker workflows: start runs,
  get/list/cancel/resume runs, send signals, watch run events, wait for terminal
  completion, claim jobs, heartbeat, complete jobs, and emit job events.
- Generated OpenAPI bindings for the full API contract when you need a lower
  level escape hatch.

All three SDKs share the same retry and rate-limit handling: `429` and
`503` responses are retried transparently (respecting `Retry-After`), and
`429`s that can't be retried surface as a typed `RateLimitError` carrying
the server's rate-limit headers. See [`docs/retries.md`](./docs/retries.md)
for the full policy.

## Development

```bash
make build-cli       # local mobius binary at bin/mobius
make release-cli VERSION=0.1.0  # cross-build release artifacts into dist/
make tools           # install codegen tools
make generate        # regenerate Go / Python / TypeScript clients from openapi.yaml
make generate-check  # verify committed generated files match the spec
make test            # run every language's test suite
```

Release tags are namespaced by package target:

- `vX.Y.Z` publishes the CLI binaries and updates the Homebrew tap.
- `npm-vX.Y.Z` publishes `@deepnoodle/mobius` to npm.
- `pypi-vX.Y.Z` publishes `deepnoodle-mobius` to PyPI.

## Security

See [SECURITY.md](SECURITY.md) for how to report vulnerabilities.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
