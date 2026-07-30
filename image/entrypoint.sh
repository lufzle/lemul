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
# (CC_REMOTE_ANALYSIS.md section 5.2). That is the mechanism pinning telemetry to
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

printf '%s' "$env_json" | jq '{env: .}' > "$SETTINGS"
chmod 0644 "$SETTINGS"

echo "entrypoint: rendered $SETTINGS" >&2
jq -r '.env | keys | join(" ")' "$SETTINGS" | sed 's/^/entrypoint: managed keys: /' >&2

exec /usr/local/bin/supervisor "$@"
