# The workspace sandbox image: one Fargate task per workspace, N Claude Code
# PTYs inside it.
#
# Promoted from spike S1, which proved Claude Code runs here against Bedrock
# with no credentials in the image -- the task role supplies them through the
# container credential provider. Nothing here is credential-bearing, and nothing
# here is tenant-specific: everything that varies is environment, rendered into
# managed settings at container start (see entrypoint.sh).
FROM node:22-slim

# ncurses-term so TERM values beyond the base set resolve inside the container.
# Without a matching terminfo entry, TUIs fall back or misbehave, and the
# transport gets blamed for it (CC_REMOTE_ANALYSIS.md section 4.1 requirement 6).
#
# jq renders managed settings at start; ripgrep and git are what Claude Code
# reaches for constantly. No tmux -- decision #4 rules it out.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git ripgrep ncurses-term jq less procps \
    && rm -rf /var/lib/apt/lists/*

# Pinned, not "latest": a customer's runner must not silently change Claude Code
# version underneath a running fleet. Rolling forward is a deliberate image
# build (section 13, "version skew").
ARG CC_VERSION=2.1.220
RUN npm install -g @anthropic-ai/claude-code@${CC_VERSION} \
    && npm cache clean --force

# The supervisor is baked in rather than mounted, so the container is the same
# artifact locally and on Fargate. Build it first:
#   GOOS=linux go build -o image/bin/supervisor ./cmd/supervisor
COPY bin/supervisor /usr/local/bin/supervisor
COPY bin/preflight /usr/local/bin/preflight
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/supervisor /usr/local/bin/preflight

# MCP config fills the WebSearch gap, the one genuine feature loss on Bedrock
# (section 3). The provider key is injected at runtime, never baked.
COPY mcp.json /etc/claude-code/mcp.json

# TERM and COLORTERM are set here as a floor; the supervisor overrides them
# per-session from what the client reported.
ENV TERM=xterm-256color \
    COLORTERM=truecolor \
    CLAUDE_CONFIG_DIR=/workspace/.claude

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
