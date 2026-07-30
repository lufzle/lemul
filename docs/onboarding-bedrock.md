# Onboarding: Bedrock model access

**Do this first, before deploying anything.** Model access is the one step with a
multi-day failure mode, and it is entirely independent of our software — which
means it can be cleared while the rest of the setup is still being planned.

Everything else in onboarding is a Terraform apply. This is the part that can
leave you waiting on AWS support, so it goes first.

## 1. Enable Anthropic models

Anthropic models on Bedrock require a **First Time Use (FTU) form**, submitted
once per AWS account in the Bedrock console under *Model access*.

Two things worth knowing before you start:

- **It can be inherited.** If the account belongs to an AWS Organization and the
  management account has already completed the form, member accounts inherit
  access. Check with whoever owns the Org before filling anything in.
- **It is granted instantly on submission** — there is no review queue for a
  well-formed submission.

### Write a substantive use-case description

The `useCases` field is the whole submission. A one-line answer is the most
common reason for a denial, and a denial is not recoverable by resubmitting (see
§3). Describe the actual workload, the data involved, and the controls around it.

Something in this shape:

> Internal software engineering assistant for our development team. Engineers run
> Claude Code in isolated, per-developer sandboxes inside our own AWS account to
> read and modify our first-party application source code, generate and review
> pull requests, write tests, and answer questions about our internal codebases.
> Inputs are our own source code, configuration and technical documentation.
> No end-customer data, no personal data, and no regulated data are sent to the
> models. Access is restricted to employees of the company, sandboxes run in a
> private VPC with egress restricted to package registries, and all model
> invocations are logged with per-user attribution for audit.

Adjust it to be true of your environment. Do not paste it unchanged if it is not.

## 2. Verify — do not assume

**Do not trust the console's green tick, and do not trust `list-inference-profiles`.**
Neither is an entitlement check. A test account listed 25 Anthropic profiles as
`ACTIVE` and could invoke none of them.

The only thing that proves access is a real invocation:

```bash
AWS_PROFILE=<your-profile> preflight -region us-east-2
```

Expected:

```
bedrock preflight (us-east-2)
  ok    opus   us.anthropic.claude-opus-5                    auth=AUTHORIZED
  ok    haiku  us.anthropic.claude-haiku-4-5-20251001-v1:0   auth=AUTHORIZED
  ok    sonnet us.anthropic.claude-sonnet-4-6                auth=AUTHORIZED

preflight passed
```

It costs three one-token invocations — a fraction of a cent.

Opus and Haiku must pass. **Haiku is not optional**: Claude Code reaches for it
on nearly every turn for cheap internal work, so an unusable Haiku degrades every
session rather than an occasional one. Sonnet only warns.

Run it **in the region the workspaces will run in**. Pins are region-specific, and
an account authorized in `us-east-1` tells you nothing about `eu-west-1`.

## 3. If access is denied

**This is the case to take seriously: a denial has no self-service recovery.**

Once denied, resubmitting does not work — `put_use_case_for_model_access` returns:

```
ValidationException: Your account is not authorized … create a support case
```

The only route forward is an **AWS support case**. Open one promptly; it is the
long pole in onboarding, and nothing else you do unblocks it.

To reduce the chance of getting here: write the `useCases` description properly
the first time (§1), and check whether the Org management account has already
completed the form before submitting from a member account.

## 4. What the preflight tells you when it fails

Failures are mapped to the actual fix, because the underlying AWS errors look
alike and are not:

| What you see | What it means | What to do |
|---|---|---|
| `the account does not have model access…` | The FTU form was never completed, or was denied | §1, then §3 if denied |
| `the task role lacks bedrock:InvokeModel…` | IAM, not model access | Fix the sandbox task role policy. Minutes. |
| `this model requires an inference profile` | You pinned a bare model ID | Use the `us.` cross-region profile ID |
| `no such model in this region` | Right pin, wrong region | Pins are region-specific |
| `throttled, which is a quota problem…` | Says nothing about access | Retry; request a quota increase if it persists |

The distinction that matters most is the first two. `AccessDeniedException` covers
both "your IAM policy is wrong" (a two-minute fix) and "your account has no model
access" (potentially days), and telling them apart is most of what the preflight
is for.

## 5. Model pins

Pinning is mandatory, not a preference. Unpinned, `opus` and `sonnet` resolve to
Claude Code's built-in Bedrock default, which can lag the current release or be
unavailable in your account — and the documentation warns that an unpinned
deployment is **billed at Opus rates**.

Defaults:

```
opus    us.anthropic.claude-opus-5
sonnet  us.anthropic.claude-sonnet-4-6
haiku   us.anthropic.claude-haiku-4-5-20251001-v1:0
```

Always the `us.` cross-region inference profile form. The bare foundation model
ID fails with *"Invocation of model ID … with on-demand throughput isn't
supported"* even on a fully authorized account.

## 6. After deployment

The same check runs inside every workspace task at start, using the **sandbox
task role** — the credential sessions actually use, which is not the one you ran
`preflight` with above. Both checks matter, and they catch different things.

If a required model is unusable, session creation is refused with the fix
attached rather than letting Claude Code start and then fail on the first prompt.
Admins can see the full per-model report at:

```
GET /v1/workspaces/{workspace_id}/preflight
```

A workspace is re-checked whenever its task restarts, so once access is granted
the recovery is simply to start a workspace again — no cache to clear.

## Two traps worth knowing

**The container credential provider requires *temporary* credentials.** A
long-term IAM user key produces `Could not load credentials from any providers`
*even when the credential endpoint was fetched successfully* — a badly misleading
error. Fargate always supplies temporary credentials, so production is fine;
this only bites in local testing. Check the credential *type* before suspecting
networking or IAM.

**Same AWS account is not the same as private networking.** Without an interface
VPC endpoint (`com.amazonaws.<region>.bedrock-runtime`), a task calling Bedrock
routes out through the IGW/NAT, across the public internet, and back into AWS.
TLS and SigV4 make it cryptographically fine, but it appears as internet egress
in flow logs and fails a "no public egress from this subnet" control.
