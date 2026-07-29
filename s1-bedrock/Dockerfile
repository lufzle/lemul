# Spike S1: Claude Code in a container against Amazon Bedrock.
#
# This is a first cut at the sandbox image that runs in the customer's VPC as a
# Fargate task. It deliberately contains no AWS credentials -- in production the
# task role supplies them via the container credential provider, and this image
# is built to work that way (see creds-shim/ for the local equivalent).
FROM node:22-slim

# ncurses-term so TERM values beyond the base set resolve inside the container.
# Without a matching terminfo entry, TUIs fall back or misbehave -- see
# CC_REMOTE_ANALYSIS.md section 4.1 requirement 6.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git ripgrep ncurses-term jq \
    && rm -rf /var/lib/apt/lists/*

ARG CC_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CC_VERSION} \
    && npm cache clean --force

# Managed settings. Env vars declared here have the highest precedence and
# cannot be overridden by the user -- this is the mechanism that pins telemetry
# to our collector (CC_REMOTE_ANALYSIS.md section 5.2). Exporter *selectors* must
# be listed too: they follow normal per-key precedence, so without them a user
# could set OTEL_LOGS_EXPORTER=none and silence a signal.
RUN mkdir -p /etc/claude-code
COPY managed-settings.json /etc/claude-code/managed-settings.json

# MCP config: fills the WebSearch gap, which is the one real feature loss on
# Bedrock. Left as a template -- the search provider key is injected at runtime.
COPY mcp.json /workspace/.mcp.json

ENV CLAUDE_CODE_USE_BEDROCK=1 \
    TERM=xterm-256color \
    COLORTERM=truecolor

WORKDIR /workspace
