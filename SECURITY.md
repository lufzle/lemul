# Security policy

This project is **alpha**. Treat it as experimental: do not run it against
workloads or credentials you cannot afford to lose.

## Reporting a vulnerability

**Do not file a public GitHub issue** for a security problem.

Use [GitHub private vulnerability reporting](https://github.com/lufzle/lemul/security/advisories/new)
so the report stays private until a fix is ready.

Include:

- a description of the issue and its impact
- steps to reproduce, or a proof of concept
- the commit SHA or release you tested

We will acknowledge reports as soon as practical. There is no bug bounty.

## Scope

In scope: the control plane, relay, runner, supervisor, CLI, console, sandbox
image, and the Terraform / deploy stacks in this repository.

Out of scope: third-party services this project talks to (Claude Code, AWS,
LiteLLM, Logto, and similar), unless the bug is in how this repository uses
them.
