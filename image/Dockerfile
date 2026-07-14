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
# transport gets blamed for it.
#
# jq renders managed settings at start; ripgrep and git are what Claude Code
# reaches for constantly. No tmux -- decision #4 rules it out.
# bubblewrap is required by CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, which strips
# provider credentials from Bash/hook/MCP subprocesses and, on Linux, runs them
# in an isolated PID namespace so they cannot read the parent's environment via
# /proc. Without it Claude Code refuses to start when the scrub is on -- it fails
# closed rather than silently running without isolation, which is the behaviour
# you want from a security control.
# acl is not optional furniture. /shared is group-writable across every member of
# a workspace, and setgid alone fixes only the GROUP of a new file -- its MODE
# comes from the creating process's umask, which every tool subprocess Claude
# Code spawns inherits from whatever the shell set. A default ACL is what makes
# "Alice creates it, Bob edits it" work, and without it that fails days later
# rather than at build time.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git ripgrep ncurses-term jq less procps bubblewrap acl \
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

# The MCP ALLOWLIST, and the filename is load-bearing.
#
# Claude Code reads /etc/claude-code/managed-mcp.json; this used to be copied to
# mcp.json, which it reads from nowhere -- so the file was inert and the empty
# server list it appeared to enforce enforced nothing.
#
# Empty on purpose. With allowManagedPermissionRulesOnly's sibling
# `allowManagedMcpServersOnly` set in the entrypoint, an empty list means NO MCP
# server can run: verified against 2.1.220 that it blocks a member's own
# user-scope `mcpServers` and a project `.mcp.json` alike, while a server listed
# here still works. It is an allowlist we own, not an off switch.
#
# MCP is deferred to a later phase (decision #5, the WebSearch gap in section 3).
# Until then this being empty is what keeps section 2.4's "no tool executing"
# unambiguous -- an MCP server is a long-lived child of a session, and 2.1.220
# starts them LAZILY, so one would read as a tool that never finishes and switch
# idle detection off for that session silently.
COPY managed-mcp.json /etc/claude-code/managed-mcp.json

# TERM and COLORTERM are set here as a floor; the supervisor overrides them
# per-session from what the client reported.
# LEMUL_SESSION_UID is the security boundary between the supervisor and the
# sessions it forks. The supervisor stays root -- it holds the gateway
# credential -- and Claude Code runs as node (uid 1000, already in the base
# image), so /proc/<supervisor>/environ is unreadable to it. Without this the
# credential is one `grep /proc/*/environ` away from every Bash tool call
# (section 12.6). The entrypoint chowns the workspace to match.
ENV TERM=xterm-256color \
    COLORTERM=truecolor \
    LEMUL_SESSION_UID=1000 \
    LEMUL_SESSION_GID=1000 \
    CLAUDE_CONFIG_DIR=/workspace/.claude

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
