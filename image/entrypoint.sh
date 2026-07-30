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

# --- Gateway (the supported inference path, decision #12) -------------------
# Nothing is written here. The supervisor brokers model traffic through a
# per-session loopback proxy and sets ANTHROPIC_BASE_URL/AUTH_TOKEN per child,
# because a value in managed settings would reach every session -- and Claude
# Code's Bash tool inherits the environment, so that is every command the agent
# runs. The credential stays in the supervisor process.

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

  # Pinning is mandatory. Unpinned, opus/sonnet resolve to Claude Code's built-in
  # Bedrock default, which can lag or be unavailable in the customer's account --
  # and the docs warn an unpinned deployment is billed at Opus rates (3.1).
  add ANTHROPIC_DEFAULT_OPUS_MODEL   "${LEMUL_OPUS_MODEL:-us.anthropic.claude-opus-5}"
  add ANTHROPIC_DEFAULT_SONNET_MODEL "${LEMUL_SONNET_MODEL:-us.anthropic.claude-sonnet-4-6}"
  add ANTHROPIC_DEFAULT_HAIKU_MODEL  "${LEMUL_HAIKU_MODEL:-us.anthropic.claude-haiku-4-5-20251001-v1:0}"
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

  # Per-tenant attribution is free: every metric and event carries these (5.3).
  attrs="tenant.id=${LEMUL_TENANT_ID:-unknown},workspace.id=${LEMUL_WORKSPACE_ID:-unknown}"
  add OTEL_RESOURCE_ATTRIBUTES "$attrs"
fi

printf '%s' "$env_json" | jq '{env: .}' > "$SETTINGS"
chmod 0644 "$SETTINGS"

echo "entrypoint: rendered $SETTINGS" >&2
jq -r '.env | keys | join(" ")' "$SETTINGS" | sed 's/^/entrypoint: managed keys: /' >&2

exec /usr/local/bin/supervisor "$@"
