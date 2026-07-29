# S1 Results — Claude Code in a container against Amazon Bedrock

**Date:** 2026-07-29 · **Claude Code:** 2.1.220 · **Runtime:** OrbStack (Docker 29.4.0)
**Account:** aureum-dev profile, `us-east-2`

**Verdict: 7/7 PASS.** Claude Code works in a container against Bedrock, the
documented feature gap is exactly one tool (WebSearch), and the Fargate
task-role credential path works with no static keys in the image.

## Did we need ECS?

**No.** Everything S1 asks is answerable under OrbStack:

| S1 question | OrbStack sufficient? |
|---|---|
| Does CC run headless against Bedrock? | ✅ |
| Do managed-settings model pins resolve? | ✅ |
| Is WebSearch really gone? | ✅ |
| Does WebFetch still work? | ✅ |
| Do MCP servers work (to fill the gap)? | ✅ |
| Does OTel escape the container to our collector? | ✅ |
| Does the **Fargate task-role** credential path work? | ✅ via a local shim |

ECS/Fargate adds only two things, neither of which changes whether Claude Code
*functions*:

- **Network path** — the `bedrock-runtime` VPC endpoint. This is a routing and
  compliance property, not a functional one. Worth validating during Phase 1
  infrastructure work, not here.
- **microVM isolation** — a security boundary, not a behavior.

The one mechanism that looked ECS-specific — task-role credentials — turned out
to be testable locally, because the AWS SDK reaches them over plain HTTP
(`AWS_CONTAINER_CREDENTIALS_FULL_URI`). Serving that from a shim on loopback
exercises the identical code path.

## Results

```
A. Inference and model pinning
  PASS  bedrock inference                  headless -p works
  PASS  explicit --model override          haiku profile accepted

B. Feature gap (the documented Bedrock limitation)
  PASS  WebSearch absent (expected)        confirms documented gap
  PASS  WebFetch works                     client-side, unaffected by Bedrock

C. MCP (fills the WebSearch gap)
  PASS  MCP server usable                  CC runs its own MCP client

D. Telemetry reaches a collector outside the container
  PASS  OTel egress from container         managed settings pinned the endpoint

E. Container credential provider (the Fargate task-role mechanism)
  PASS  task-role credential provider      no static keys in the container
```

Reproduce: `./run-s1.sh aureum-dev-full-access us-east-2`
(run `otel-probe -addr :14318` first for test D, else it SKIPs).

## The finding worth carrying into Phase 1

### The task-role credential path requires *temporary* credentials

The first attempt failed with:

```
API Error: Could not load credentials from any providers
```

…while the shim log showed the SDK **hitting the endpoint 13 times and
retrying**. So the provider was selected and the request succeeded — the
*response* was rejected.

Cause: the profile is a long-term IAM user key with no session token, so the
shim served a response with no `Token` field. The HTTP credential provider
requires one. Serving real temporary credentials (`sts get-session-token`) made
it pass immediately.

**Why this matters beyond the test:** Fargate always supplies temporary
credentials, so production is fine. But the failure mode is badly misleading —
"could not load credentials from any providers" reads like the endpoint is
unreachable or the env var is wrong, when in fact the fetch succeeded and only
the payload shape was wrong. Anyone debugging a credential problem in the
sandbox should check **whether the served credentials are temporary** before
suspecting networking or IAM.

Corollary: you cannot test this path with a long-term IAM user key. Mint temp
credentials, or assume a role.

## Confirmed: the gap is one tool

The docs list a lot as unsupported on Bedrock. Empirically, for Claude Code it
comes to one thing:

| Documented as unsupported | Actual effect on CC |
|---|---|
| Server-side code execution | none — the sandbox has bash |
| Agent Skills (`container.skills`) | none — CC loads skills from `.claude/skills/` |
| MCP connector (`mcp_servers` API param) | **none — verified.** CC's own MCP client works |
| Files API, Batches, Models API | none for CC |
| Server-side `web_fetch` | **none — verified.** CC's WebFetch is client-side |
| **WebSearch** | **real, and the only one** |

So a search MCP server is the whole mitigation. Not yet wired up here — it needs
a provider key (Brave/Tavily/Exa), which is open decision #5 in
`CC_REMOTE_ANALYSIS.md` §12. The MCP mechanism itself is proven (test C used
`@modelcontextprotocol/server-filesystem` to avoid needing a key).

## Model pinning verified through telemetry

OTel `api_request` events confirm the managed-settings pins resolved to the
intended profiles rather than a stale Claude Code default:

```
model  us.anthropic.claude-opus-5
model  us.anthropic.claude-haiku-4-5-20251001-v1:0
```

This matters because an unpinned deployment falls back to CC's built-in Bedrock
default, which can lag or be unavailable in the customer's account — and the
docs warn it gets **billed at Opus rates**. Pins belong in managed settings
(where users cannot override them), which is where this image puts them.

Note the same split S2 found: metric-level `model` carries the Bedrock profile
ID, event-level carries the bare ID (`claude-opus-5`). Normalize when
aggregating.

## Image notes

`node:22-slim` + `@anthropic-ai/claude-code`, plus `git`, `ripgrep`, `jq`,
`curl`, and **`ncurses-term`** — the last so `TERM` values beyond the base set
resolve inside the container (§4.1 requirement 6).

**Contains no credentials.** Managed settings at
`/etc/claude-code/managed-settings.json` pin the model IDs and the OTel
endpoint; both are user-unoverridable, which is the property Phase 1 depends on.

## Addendum — model access, and the Mantle endpoint (2026-07-29)

### Claude Code supports Mantle natively — verified

```bash
CLAUDE_CODE_USE_MANTLE=1                      # Messages-API Bedrock endpoint
# model IDs: anthropic.claude-sonnet-5  (anthropic. prefix, no version suffix)
ANTHROPIC_BEDROCK_MANTLE_BASE_URL=...         # override for a gateway
CLAUDE_CODE_SKIP_MANTLE_AUTH=1                # gateway injects SigV4 server-side
```

In Mantle mode CC reached the endpoint and returned a genuinely useful error:

```
The model anthropic.claude-sonnet-5 is not available on your bedrock deployment.
Try --model to switch to us.anthropic.claude-sonnet-4-6, or ask your admin to
enable this model.
```

**Dual mode works and is the right production config.** With both
`CLAUDE_CODE_USE_BEDROCK=1` and `CLAUDE_CODE_USE_MANTLE=1`, CC routes by model-ID
shape — `anthropic.*` to Mantle, everything else to the Invoke API. Verified:
`us.anthropic.claude-haiku-4-5-20251001-v1:0` returned `DUAL_OK`.

### But Mantle is *worse* onboarding friction, not better

The AWS docs say Mantle is exempt from the Anthropic First Time Use form, which
looks like it removes an onboarding step per customer. Empirically it does not:

| Account | Invoke API | Mantle |
|---|---|---|
| aureum-dev | ✅ works | SigV4 **accepted**, but 404 `does not exist` for every model ID tried — zero models granted |
| dario | ❌ `Operation not allowed` (no FTU form) | ❌ 403 `not available for this account … contact AWS Sales` |

`dario` has **AdministratorAccess**, so neither failure is IAM.

So the trade is: the Invoke API needs a **self-service form that grants access
immediately**, while Mantle needs an **AWS account-team conversation**. For
onboarding a customer, the form is strictly better. Mantle's own model lineup is
also separate from the Bedrock catalog and account-specific.

**Conclusion: plan on the Invoke API. Set both flags anyway** so a customer who
*is* Mantle-entitled works without a config change.

### `list-inference-profiles` is not an entitlement check

`dario` lists 25 Anthropic inference profiles, all `ACTIVE`, including Sonnet 5
and Fable 5 — and cannot invoke a single one. `ACTIVE` reports that the profile
exists in the region, nothing about your account's access. Only an invoke tells
you.

**This must become a runner startup preflight** (see CC_REMOTE_ANALYSIS.md §8).
Without it, a customer whose account lacks model access gets an opaque
`Operation not allowed` mid-session instead of a clear failure at onboarding.

### FTU denial is not self-recoverable — an onboarding risk

Correcting an earlier read in this spike: `dario`'s form was **not missing, it
was submitted and denied.** `get_use_case_for_model_access` returns it:

```json
{"companyName":"Luis Farzati","companyWebsite":"https://linkedin.com/in/luisfarzati",
 "intendedUsers":"1","industryOption":"Software as a Service","useCases":". Lab"}
```

`get_foundation_model_availability` shows why nothing invokes — and note that
only `authorizationStatus` differs from the working account:

| Field | dario | aureum-dev (works) |
|---|---|---|
| `agreementAvailability` | `AVAILABLE` (haiku) / `NOT_AVAILABLE` (opus-5) | `AVAILABLE` |
| **`authorizationStatus`** | **`NOT_AUTHORIZED`** | `AUTHORIZED` |
| `entitlementAvailability` | `AVAILABLE` | `AVAILABLE` |
| `regionAvailability` | `AVAILABLE` | `AVAILABLE` |

aureum-dev's form has a substantive `useCases` paragraph; dario's is `". Lab"`.
The docs state access is "granted or denied based on your answers," so an
inadequate description appears to be the cause.

**And it cannot be fixed by resubmitting.** `put_use_case_for_model_access`
returns:

```
ValidationException: Your account is not authorized to perform this action.
Please create a support case ... and we will get back to you.
```

So once denied, the account is locked out of self-service and needs an AWS
support case.

**Product consequence:** the happy path (form → instant access) has a failure
mode with no self-service recovery. A customer who fills the form carelessly is
stuck behind an AWS support ticket, and our onboarding will look broken through
no fault of ours. The onboarding runbook must:

1. Tell them to write a substantive `useCases` description, with example wording.
2. Verify with `get_foundation_model_availability` and check
   **`authorizationStatus == AUTHORIZED`** — not just that the model is listed.
3. Detect `NOT_AUTHORIZED` and surface the support-case path explicitly, rather
   than letting them hit `Operation not allowed` and blame us.

Note this is a *different* diagnostic than the `Operation not allowed` invoke
error: `get_foundation_model_availability` distinguishes "no agreement" from
"not authorized" from "wrong region", which the invoke error does not. The
preflight should use it.

### Enabling access on an un-entitled account

Anthropic models need the FTU form once per account (or once at the AWS
Organization management account, which then inherits to members):

- Console: Bedrock → Model catalog → pick an Anthropic model → submit use-case
  form. Access is granted immediately.
- CLI: `aws bedrock put-use-case-for-model-access --form-data <base64 json>`,
  then `get-foundation-model-availability` to confirm.
  **Requires AWS CLI ≥ 2.27.42** — this machine has 2.23.15, so those
  subcommands are absent. That is why the earlier `get-use-case-for-model-access`
  probe returned a usage error rather than an answer.

Also required: `aws-marketplace:Subscribe` / `Unsubscribe` /
`ViewSubscriptions`, and a valid Marketplace payment method.

## Not covered

- **VPC endpoint / PrivateLink** — network path, Phase 1 infrastructure.
- **Scoped IAM policy validation.** These tests ran with a full-access profile,
  so they prove CC *works*, not that the minimal
  `bedrock:InvokeModel` + `InvokeModelWithResponseStream` +
  `ListInferenceProfiles` + `GetInferenceProfile` policy is sufficient. Worth an
  explicit test with a scoped role — it needs creating an IAM role, so it was
  left out of a read-only spike.
- **Managed-settings *enforcement*.** The settings are read and applied; the
  v2.1.217+ behavior where they *strip* a user's conflicting
  `OTEL_EXPORTER_OTLP_*` vars is untested.
- **Long-session credential refresh.** CC caches credentials until 5 minutes
  before expiry; a session outliving a 1-hour STS credential is untested.
- **Real Fargate.** Image size, cold start, and microVM behavior.
