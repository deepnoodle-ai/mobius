# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Mobius, please report it privately so we
can address it before public disclosure.

**Please do not open a public GitHub issue for security reports.**

Email: **security@mobiusops.ai**

Include as much of the following as you can:

- A description of the issue and its impact
- Steps to reproduce (proof-of-concept code, if available)
- The affected component (Go core, Python SDK, or TypeScript SDK) and version
- Any suggested remediation

We will acknowledge your report within 3 business days and aim to provide a resolution
timeline within 7 business days. Once a fix is released, we are happy to credit you in
the release notes (opt-in).

## Worker credential exposure to sandboxed commands

The `environment.bash` stock action (and the git actions) run commands with the
worker's full process environment, **including `MOBIUS_API_KEY`**. This is
intentional: a managed environment is single-tenant — it exists to run one
project's jobs — and agent scripts routinely invoke the `mobius` CLI, which
needs the credential. Treat anything executed inside a worker environment as
having the worker's project-scoped access. The `environment.logs.tail` action
redacts credential-shaped tokens (`mbx_…`, GitHub PATs) from returned log
content, but that is output hygiene, not an isolation boundary. If you embed
the SDK in a multi-tenant host, do not register the stock environment actions,
or scrub the environment before spawning workers.

## Scope

This policy covers the code published from this repository:

- `github.com/deepnoodle-ai/mobius`
- `deepnoodle-mobius` on PyPI
- `@deepnoodle/mobius` on npm

For vulnerabilities in the Mobius service itself (api.mobiusops.ai), please use the
same email address.
