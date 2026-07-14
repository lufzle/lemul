#!/bin/sh
# Renders Claude Code's managed settings from the environment, then hands off to
# the supervisor.
#
# Rendered at start rather than baked into the image for one concrete reason:
# the same image has to serve a Bedrock workspace in a customer's account AND
# local development against a host login, and those need DIFFERENT managed
# settings -- the local one must not set CLAUDE_CODE_USE_BEDROCK at all. Baking
# them would mean two images, which is exactly the drift the driver interface
# exists to prevent.
#
# Managed settings are the admin-enforced layer: env declared here has the
# highest precedence and a user inside the session cannot override it
# (managed-settings precedence). That is the mechanism pinning telemetry to
# our collector and pinning the models.
set -eu

SETTINGS_DIR=/etc/claude-code
SETTINGS=$SETTINGS_DIR/managed-settings.json
mkdir -p "$SETTINGS_DIR"

# Start from the settings that are true regardless of deployment.
#
# The four content flags are off, and off EXPLICITLY: section 5.4 notes that
# OTEL_LOG_ASSISTANT_RESPONSES falls back to OTEL_LOG_USER_PROMPTS when unset,
# so leaving it out would silently start logging responses the day someone turns
# prompt logging on. "We can prove we never collect your prompts" is worth more
# than any dashboard feature.
env_json=$(jq -n '{
  OTEL_LOG_USER_PROMPTS: "0",
  OTEL_LOG_ASSISTANT_RESPONSES: "0",
  OTEL_LOG_TOOL_DETAILS: "0",
  OTEL_LOG_RAW_API_BODIES: "0"
}')

add() { env_json=$(printf '%s' "$env_json" | jq --arg k "$1" --arg v "$2" '. + {($k): $v}'); }

# --- Model pins -------------------------------------------------------------
# Pinning is mandatory in every mode. Unpinned, opus/sonnet resolve to Claude
# Code's built-in default, which can lag, can be unavailable, and which the docs
# warn is billed at Opus rates (section 3.1).
#
# LEMUL_PINS is "role=modelID" pairs, the same value the preflight checks, so one
# setting cannot drift from the other. The IDs are provider-scoped: Bedrock wants
# inference profile IDs (us.anthropic.…) while a gateway's are arbitrary aliases
# defined in its own config, so there is no universal default -- an unset pin is
# left unset rather than guessed.
pin_from_spec() {
  # Trailing newline matters: without it `read` drops the final field, which
  # silently loses the last pin.
  printf '%s\n' "${LEMUL_PINS:-}" | tr ',' '\n' | while IFS='=' read -r role id; do
    [ -n "$role" ] && [ -n "$id" ] && printf '%s\t%s\n' "$role" "$id"
  done
}
for role in opus sonnet haiku; do
  id=$(pin_from_spec | awk -F'\t' -v r="$role" '$1==r {print $2; exit}')
  [ -z "$id" ] && continue
  case "$role" in
    opus)   add ANTHROPIC_DEFAULT_OPUS_MODEL   "$id" ;;
    sonnet) add ANTHROPIC_DEFAULT_SONNET_MODEL "$id" ;;
    haiku)  add ANTHROPIC_DEFAULT_HAIKU_MODEL  "$id" ;;
  esac
done

# --- Gateway (the supported inference path, decision #12) -------------------
# No base URL or token is written here. The supervisor brokers model traffic
# through a per-session loopback proxy and sets ANTHROPIC_BASE_URL/AUTH_TOKEN per
# child, because a value in managed settings would reach every session -- and
# Claude Code's Bash tool inherits the environment, so that is every command the
# agent runs. The credential stays in the supervisor process.
#
# What DOES belong here are the behaviours Claude Code changes when it detects a
# non-first-party endpoint. Several default OFF behind a gateway and their
# absence looks like our bug rather than a default (§12.6).
if [ -n "${LEMUL_GATEWAY_URL:-}" ]; then
  # Off by default on gateway connections. Without it a large tool input -- a long
  # file write, say -- arrives only once fully generated, which reads as the TUI
  # hanging. That would land on us as "the proxy is buffering".
  add CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING 1

  # When a streaming request fails mid-stream, Claude Code retries non-streaming.
  # Behind a proxy that can replay a partially-processed request, the retry
  # produces DUPLICATE TOOL EXECUTION -- the same command run twice. Defensive.
  add CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK 1

  # Trace context only propagates to a custom base URL when asked, and every
  # session's traffic goes through our loopback broker.
  add CLAUDE_CODE_PROPAGATE_TRACEPARENT 1

  # Situational, so opt-in rather than assumed:
  #   pins are gateway ALIASES, which Claude Code cannot recognise as
  #   effort-capable, so the effort parameter is dropped unless forced
  [ "${LEMUL_GATEWAY_FORCE_EFFORT:-}" = "1" ] && add CLAUDE_CODE_ALWAYS_ENABLE_EFFORT 1
  #   context window cannot be inferred from an alias either
  [ -n "${LEMUL_GATEWAY_CONTEXT_TOKENS:-}" ] && add CLAUDE_CODE_MAX_CONTEXT_TOKENS "$LEMUL_GATEWAY_CONTEXT_TOKENS"
  #   only if the gateway rejects anthropic-beta headers or caches on the body
  [ "${LEMUL_GATEWAY_NO_BETAS:-}" = "1" ] && add CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS 1
  [ "${LEMUL_GATEWAY_NO_ATTRIBUTION:-}" = "1" ] && add CLAUDE_CODE_ATTRIBUTION_HEADER 0
  #   populates /model from the gateway; off by default because a shared key
  #   would otherwise show every user every model the key can reach
  [ "${LEMUL_GATEWAY_MODEL_DISCOVERY:-}" = "1" ] && add CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY 1
fi

# --- Bedrock (DRAFT -- not the supported path) ------------------------------
# Direct-to-Bedrock is deferred: the task role is reachable from any process in
# the task, so a session's Bash tool can invoke Bedrock outside our accounting
# and can exfiltrate the credentials. Kept for reconsideration; see decision #12.
# Absent means this workspace is not using Bedrock (local development against a
# host login). The flag must then be absent entirely, not set to "0".
if [ "${LEMUL_BEDROCK:-}" = "1" ]; then
  add CLAUDE_CODE_USE_BEDROCK 1
  # Both flags on purpose (section 3.2): a Mantle-entitled customer then works
  # with no config change, and Claude Code routes by model-ID shape.
  add CLAUDE_CODE_USE_MANTLE 1

  # Pins are set above from LEMUL_PINS, in every mode.
fi

# --- Telemetry -------------------------------------------------------------
# The exporter SELECTORS are set alongside the endpoint deliberately. Section 5.2
# records the gap: selectors follow normal per-key precedence, so without them a
# user could set OTEL_LOGS_EXPORTER=none and silence a signal we rely on for
# billing and idle detection.
if [ -n "${LEMUL_OTEL_ENDPOINT:-}" ]; then
  add CLAUDE_CODE_ENABLE_TELEMETRY 1
  add OTEL_METRICS_EXPORTER otlp
  add OTEL_LOGS_EXPORTER otlp
  add OTEL_EXPORTER_OTLP_PROTOCOL "${LEMUL_OTEL_PROTOCOL:-http/json}"
  add OTEL_EXPORTER_OTLP_ENDPOINT "$LEMUL_OTEL_ENDPOINT"
  [ -n "${LEMUL_OTEL_HEADERS:-}" ] && add OTEL_EXPORTER_OTLP_HEADERS "$LEMUL_OTEL_HEADERS"

  # Traces are BETA in Claude Code and higher volume than metrics or events, so
  # they are opt-in rather than part of the default telemetry block. The sampler
  # is pinned explicitly: leaving it to the default risks a low sample rate that
  # looks like traces are broken rather than sampled.
  if [ "${LEMUL_OTEL_TRACES:-}" = "1" ]; then
    add OTEL_TRACES_EXPORTER otlp
    add OTEL_TRACES_SAMPLER "${LEMUL_OTEL_TRACES_SAMPLER:-always_on}"
    add OTEL_TRACES_EXPORT_INTERVAL "${LEMUL_OTEL_TRACES_INTERVAL:-5000}"
  fi

  # Export intervals AND the shutdown timeout. The last one is the load-bearing
  # setting: its default of 2000 ms is not enough for a turn's final batch to
  # flush, so api_request and assistant_response -- which fire at END of turn --
  # were silently lost while user_prompt, which fires at the start, arrived. That
  # looked like Claude Code not emitting events at all.
  add OTEL_METRIC_EXPORT_INTERVAL "${LEMUL_OTEL_METRIC_INTERVAL:-10000}"
  add OTEL_LOGS_EXPORT_INTERVAL "${LEMUL_OTEL_LOGS_INTERVAL:-2000}"
  add CLAUDE_CODE_OTEL_SHUTDOWN_TIMEOUT_MS "${LEMUL_OTEL_SHUTDOWN_MS:-15000}"

  # Exporter failures are otherwise silent unless --debug is on, which is no use
  # in a sandbox nobody is watching.
  add CLAUDE_CODE_OTEL_DIAG_STDERR 1

  # Per-tenant attribution is free: every metric, event and span carries these (5.3).
  attrs="tenant.id=${LEMUL_TENANT_ID:-unknown},workspace.id=${LEMUL_WORKSPACE_ID:-unknown}"
  add OTEL_RESOURCE_ATTRIBUTES "$attrs"
fi

# --- Shared workspace layout -------------------------------------------------
# A workspace is a shared machine (section 2.3): several members of one
# organization hold sessions in this task, each at their own uid with their own
# home. Two directories have to exist before any of them arrives.
#
# Homes are NOT created here -- at container start nothing knows who the members
# are, and membership changes without a restart. The supervisor makes each one
# on that member's first session (internal/supervisor/identity.go). All this does
# is prepare the ground they go in.
HOMES_ROOT=${LEMUL_HOMES_ROOT:-/workspace/homes}
# UNDER /workspace, deliberately, and moved there on 2026-08-02.
#
# It was /shared, at the container root, while the homes sat on /workspace --
# and the argument for putting the homes there applies to this directory word
# for word: a replacement task comes up with an empty container filesystem, and
# a repo or a dataset that did not survive one is exactly what a member would
# lose. One mount point has to cover everything durable, or a volume covers half
# the workspace and a snapshot silently misses the other half.
SHARED_DIR=${LEMUL_SHARED_DIR:-/workspace/shared}
SHARED_GROUP=lemul

# 0755 root-owned: only the supervisor writes here, and a member must not be
# able to plant a directory where somebody else's home is about to go.
mkdir -p "$HOMES_ROOT"
chmod 0755 "$HOMES_ROOT"

# /shared is the one place members share. What goes in it is the customer's
# business -- a repo, a dataset, build artifacts, nothing.
#
# setgid (2775) plus a DEFAULT ACL, not chmod 777. Without both, a file Alice
# creates in /shared lands 0644 alice:alice and Bob can read it but not write
# it -- and that surfaces days later, the first time somebody edits a
# colleague's file. setgid fixes the GROUP of new entries; the default ACL is
# what fixes their MODE, because umask is per-process and every tool subprocess
# Claude Code spawns inherits whatever the login shell happened to set.
getent group "$SHARED_GROUP" >/dev/null 2>&1 || groupadd "$SHARED_GROUP"
mkdir -p "$SHARED_DIR"
chgrp "$SHARED_GROUP" "$SHARED_DIR"
chmod 2775 "$SHARED_DIR"
if command -v setfacl >/dev/null 2>&1; then
  setfacl -d -m "g:${SHARED_GROUP}:rwx" "$SHARED_DIR" 2>/dev/null &&
    echo "entrypoint: $SHARED_DIR default ACL set" >&2 ||
    echo "entrypoint: WARNING setfacl failed; files in $SHARED_DIR may not be group-writable" >&2
else
  echo "entrypoint: WARNING setfacl not installed; files in $SHARED_DIR may not be group-writable" >&2
fi

# Workspace-wide skills an owner installs, surfaced to every member.
#
# ROOT-OWNED, and that is the point rather than tidiness: a skill is instruction
# content that runs in every member's session, so a directory members could write
# would let any one of them plant something every colleague's agent then loads.
# Installing into it is a privileged act that goes through the supervisor.
WS_SKILLS=${LEMUL_WORKSPACE_SKILLS:-/opt/lemul/workspace-skills}
mkdir -p "$WS_SKILLS"
chmod 0755 "$WS_SKILLS"

# --- Managed settings --------------------------------------------------------
# The admin-enforced layer, and the only one a member cannot edit: everything
# else Claude Code reads lives in a home they own. Verified against the pinned
# 2.1.220 binary rather than the docs -- see the journal for 260801.
#
# allowManagedPermissionRulesOnly is LOAD-BEARING. Permission rules MERGE across
# scopes, which is a documented exception to the precedence order, so without it
# a member adds their own `allow` in ~/.claude/settings.json and the boundary
# leaks by design.
#
# disableSideloadFlags rejects --mcp-config, --plugin-dir and --plugin-url,
# which otherwise walk around all of it from the command line.
#
# allowManagedMcpServersOnly is the same idea for MCP, and it is set as depth
# rather than as the mechanism. BE PRECISE ABOUT WHICH IS WHICH -- getting this
# backwards is what left the hole in the first place.
#
# What actually enforces "no MCP server may run" is the mere EXISTENCE of
# /etc/claude-code/managed-mcp.json. Measured against 2.1.220, all four
# combinations:
#
#   flag absent  + file absent   -> a member's own server RUNS
#   flag absent  + file present  -> refused
#   flag present + file absent   -> a member's own server RUNS
#   flag present + file present  -> refused
#
# So the file is load-bearing and this flag has no demonstrated effect on its
# own. It is kept because the binary clearly intends it for this and it may
# cover routes the matrix did not exercise (plugin- and SDK-provided servers),
# and it is harmless -- but nothing here should be read as resting on it.
#
# With the file present and its list empty, a member's user-scope `mcpServers`
# and a project `.mcp.json` are both refused; a server listed IN the file still
# runs. It is an allowlist we own, not an off switch.
#
# This one matters beyond the boundary. An MCP server is a long-lived child of
# a session, and 2.1.220 starts them LAZILY -- long after any startup window --
# so one running under a session reads as a tool that never finishes, and
# section 2.4's idle detection quietly stops stopping anything. That is a
# workspace billing indefinitely, not a permissions question.
#
# NOTE for whoever adds the search MCP server (decision #5, section 3's
# WebSearch gap): fix supervisor/activity.go first, to exclude MCP descendants
# by their configured command. managed-mcp.json is that command list.
#
# What this is NOT: the isolation. Claude Code's permission system governs what
# the AGENT reaches for. A member with a Bash tool is a member with a shell, so
# what actually keeps them out of a colleague's home is uid plus mode 0700. This
# is a guardrail and a way to avoid an approval prompt on every /shared access.
settings=$(jq -n \
  --argjson env "$env_json" \
  --arg shared "$SHARED_DIR" \
  --arg skills "$WS_SKILLS" \
  --arg homes "$HOMES_ROOT" '{
    env: $env,
    allowManagedPermissionRulesOnly: true,
    allowManagedMcpServersOnly: true,
    disableSideloadFlags: true,
    permissions: {
      additionalDirectories: [$shared, $skills],
      deny: ["Read(" + $homes + "/**)", "Edit(" + $homes + "/**)"]
    }
  }')
printf '%s' "$settings" > "$SETTINGS"
chmod 0644 "$SETTINGS"
echo "entrypoint: rendered $SETTINGS" >&2
jq -r '.env | keys | join(" ")' "$SETTINGS" | sed 's/^/entrypoint: managed keys: /' >&2

# ASSERT the file took effect rather than assuming it did.
#
# Two failure modes, both silent, both measured against 2.1.220: a bad VALUE
# makes Claude Code warn and then IGNORE that field while carrying on, and an
# unrecognised KEY NAME is accepted without a word. Either leaves a workspace
# running with no boundary and nothing in the log to say so -- the orgslug
# lesson in a new place. `claude doctor` reads the settings files, needs no
# credentials and makes no model call, so it is the cheapest thing that can tell
# us.
#
# Fatal on purpose. A task that cannot enforce the boundary should not accept
# sessions; failing here costs a placement, and not failing costs a member's
# conversations.
if doctor=$(claude doctor 2>&1); then
  if printf '%s' "$doctor" | grep -q "schema validation"; then
    echo "entrypoint: FATAL managed settings failed Claude Code's schema validation:" >&2
    printf '%s\n' "$doctor" | grep -i "detail\|schema validation" >&2
    exit 1
  fi
  echo "entrypoint: managed settings accepted by claude doctor" >&2
else
  # doctor itself failing is not proof the settings are bad, and refusing to
  # start over a diagnostic that would not run is its own outage.
  echo "entrypoint: WARNING could not run claude doctor to verify managed settings" >&2
fi

# --- Session uid boundary ----------------------------------------------------
# The supervisor stays root: it needs the gateway credential, cgroup reads and
# PTY allocation. Sessions do not, and must not share its uid -- the credential
# lives in the supervisor's environment, and /proc/<pid>/environ is mode 0400
# owned by that uid, so a session at the SAME uid reads it straight back out.
# Measured: a session at the same uid can read the supervisor's environ.
#
# This is the boundary rather than CLAUDE_CODE_SUBPROCESS_ENV_SCRUB because the
# scrub needs bubblewrap, which needs namespace syscalls Fargate does not permit.
# A uid difference needs nothing from the platform.
#
# LEMUL_SESSION_UID is now the FLOOR rather than the answer. Each member's
# sessions run at their own uid above it, assigned by the Management API and
# provisioned by the supervisor; this value is what an unprivileged supervisor
# falls back to, and what says the boundary is meant to be on at all.
if [ "${LEMUL_SESSION_UID:-0}" != "0" ] && [ "$(id -u)" = "0" ]; then
  echo "entrypoint: sessions run at per-member uids from ${LEMUL_SESSION_UID} up" >&2
else
  echo "entrypoint: WARNING sessions share the supervisor's uid; the gateway key is readable from /proc" >&2
fi

exec /usr/local/bin/supervisor "$@"
