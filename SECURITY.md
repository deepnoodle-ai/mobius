# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Mobius, please report it privately so we
can address it before public disclosure.

**Please do not open a public GitHub issue for security reports.**

Email: **security@mobiusops.com**

Include as much of the following as you can:

- A description of the issue and its impact
- Steps to reproduce (proof-of-concept code, if available)
- The affected component (Go core, Python SDK, or TypeScript SDK) and version
- Any suggested remediation

We will acknowledge your report within 3 business days and aim to provide a resolution
timeline within 7 business days. Once a fix is released, we are happy to credit you in
the release notes (opt-in).

## Worker credential exposure to sandboxed commands

Commands a job starts (`environment.bash`, coding agents, git and its hooks)
run with the worker's process environment, but **not with `MOBIUS_API_KEY`**.
`mobius worker` removes the key from its own environment at startup, so no
child inherits it. On Linux it also marks itself non-dumpable, so a child
running as the same user cannot read the key from `/proc/<pid>/environ`. A
child running as root, or able to use passwordless `sudo`, still can, and can
also read any key file the worker was started from. Do not give job commands
root or passwordless `sudo` if the key matters to you.

Git in a job shell authenticates through `mobius git-credential-helper`. In a
managed environment the worker serves credentials on a Unix socket in a private
`0700` directory and names it in `MOBIUS_GIT_CREDENTIAL_SOCKET`. The job's
lease is not in any job's environment. The worker reads the calling process's
ID from the socket (`SO_PEERCRED` on Linux, `LOCAL_PEERPID` on macOS) and walks
its parent processes up to the command of a running job, then brokers a
repository-scoped GitHub token for that job's lease. A process outside every
running job's tree gets nothing. On other operating systems the worker
cannot tell which process is calling, so it refuses Git credentials and
action calls. Job scripts never receive the API key and cannot call the
`mobius` CLI with it.

Workflow script steps call their declared actions the same way, through a
second private socket, `MOBIUS_ACTION_SOCKET`. Mobius runs only the actions the
running script step declared, as the workflow, while the step runs.

What this does and does not protect: the process check keeps casual misuse
out, but it is not a boundary between jobs. Job commands run as the worker's
user, and in a managed computer that user can use `sudo`. So **while a script
step runs, any code in the run's computer can make that step's declared
calls.** That includes an assistant's commands in the same run. The step's
declared list limits what such code can do, Mobius checks each call against
the workflow's account access, and every call is recorded under the step.

The `environment.logs.tail` action redacts credential-shaped tokens (`mbx_…`,
GitHub PATs) from returned log content, but that is output hygiene, not an
isolation boundary. If you embed the stock environment actions in your own
host process, remove credentials from that process's environment before it
runs jobs, as `mobius worker` does.

## Scope

This policy covers the code published from this repository:

- `github.com/deepnoodle-ai/mobius`
- `deepnoodle-mobius` on PyPI
- `@deepnoodle/mobius` on npm

For vulnerabilities in the Mobius service itself (api.mobiusops.com), please use the
same email address.
