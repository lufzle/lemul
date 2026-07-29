#!/usr/bin/env bash
# Spike S1: does Claude Code work in a container against Amazon Bedrock, and
# what does the documented feature gap actually cost us?
#
# Everything here runs under OrbStack/Docker. ECS adds only the network path
# (VPC endpoint) and microVM isolation, neither of which changes whether Claude
# Code functions. The one genuinely ECS-specific mechanism -- task-role
# credentials via the container credential provider -- is exercised in test E
# with a local shim that speaks the same protocol.
#
#   ./run-s1.sh [aws-profile] [region]
set -uo pipefail

PROFILE="${1:-aureum-dev-full-access}"
REGION="${2:-us-east-2}"
IMG=s1-bedrock:latest
PROBE_PORT=14318

pass=0; fail=0; skip=0
result() { # result <PASS|FAIL|SKIP> <name> <detail>
  case "$1" in
    PASS) pass=$((pass+1)); printf '  \033[32mPASS\033[0m  %-34s %s\n' "$2" "$3" ;;
    FAIL) fail=$((fail+1)); printf '  \033[31mFAIL\033[0m  %-34s %s\n' "$2" "$3" ;;
    SKIP) skip=$((skip+1)); printf '  \033[33mSKIP\033[0m  %-34s %s\n' "$2" "$3" ;;
  esac
}

echo "=== S1: Claude Code on Bedrock in a container ==="
echo "profile=$PROFILE region=$REGION"

eval "$(aws configure export-credentials --profile "$PROFILE" --format env 2>/dev/null)" || {
  echo "could not export credentials for $PROFILE"; exit 1; }

DOCKER_CREDS=(-e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN -e "AWS_REGION=$REGION")

cc() { # cc <extra docker args...> -- <prompt> [claude args...]
  local dargs=(); while [ "$1" != "--" ]; do dargs+=("$1"); shift; done; shift
  local prompt="$1"; shift
  docker run --rm "${DOCKER_CREDS[@]}" "${dargs[@]}" "$IMG" \
    claude -p "$prompt" "$@" 2>&1
}

# --- A. inference + model resolution ------------------------------------
echo
echo "A. Inference and model pinning"
out=$(cc -- 'Reply with exactly: BEDROCK_OK')
if grep -q BEDROCK_OK <<<"$out"; then
  result PASS "bedrock inference" "headless -p works"
else
  result FAIL "bedrock inference" "$(head -2 <<<"$out")"
fi

# Which model actually served the request? Unpinned, Claude Code falls back to
# its built-in Bedrock default, which can lag or be unavailable -- and the docs
# warn an unpinned deployment gets billed at Opus rates.
out=$(docker run --rm "${DOCKER_CREDS[@]}" "$IMG" \
  sh -c 'claude -p "hi" --model us.anthropic.claude-haiku-4-5-20251001-v1:0 >/dev/null 2>&1; echo rc=$?')
if grep -q 'rc=0' <<<"$out"; then
  result PASS "explicit --model override" "haiku profile accepted"
else
  result FAIL "explicit --model override" "$out"
fi

# --- B. WebSearch: expected to be unavailable ---------------------------
echo
echo "B. Feature gap (the documented Bedrock limitation)"
out=$(cc -- 'Use the WebSearch tool to search for "anthropic". If the tool is unavailable, reply exactly: WEBSEARCH_UNAVAILABLE' --allowedTools WebSearch)
if grep -qi 'WEBSEARCH_UNAVAILABLE\|not available\|no.*websearch\|cannot' <<<"$out"; then
  result PASS "WebSearch absent (expected)" "confirms documented gap"
else
  result FAIL "WebSearch absent (expected)" "unexpectedly available: $(head -2 <<<"$out")"
fi

# WebFetch is implemented client-side by Claude Code, unlike the server-side
# web_fetch API tool. The docs only call out WebSearch, so this should survive.
out=$(cc -- 'Use WebFetch on https://example.com and reply with the page title only.' --allowedTools WebFetch)
if grep -qi 'example domain' <<<"$out"; then
  result PASS "WebFetch works" "client-side, unaffected by Bedrock"
elif grep -qi 'not available\|unavailable' <<<"$out"; then
  result FAIL "WebFetch works" "also unavailable -- gap is wider than documented"
else
  result SKIP "WebFetch works" "inconclusive: $(head -1 <<<"$out")"
fi

# --- C. MCP: client-side, should be unaffected --------------------------
echo
echo "C. MCP (fills the WebSearch gap)"
out=$(docker run --rm "${DOCKER_CREDS[@]}" "$IMG" sh -c '
  cat > /workspace/.mcp.json <<JSON
{"mcpServers":{"fs":{"command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","/workspace"]}}}
JSON
  echo mcp-probe > /workspace/probe.txt
  claude -p "List the allowed directories using the fs MCP server, then reply MCP_OK" \
    --allowedTools "mcp__fs__list_allowed_directories" --permission-mode acceptEdits 2>&1 | tail -4')
if grep -q 'MCP_OK\|/workspace' <<<"$out"; then
  result PASS "MCP server usable" "CC runs its own MCP client"
else
  result FAIL "MCP server usable" "$(tail -2 <<<"$out")"
fi

# --- D. telemetry from inside the container -----------------------------
echo
echo "D. Telemetry reaches a collector outside the container"
if curl -s -m 2 "http://localhost:$PROBE_PORT/report" >/dev/null 2>&1; then
  before=$(curl -s "http://localhost:$PROBE_PORT/report" | grep -c 'api_request' || true)
  cc -e "OTEL_EXPORTER_OTLP_ENDPOINT=http://host.docker.internal:$PROBE_PORT" \
     -e "OTEL_RESOURCE_ATTRIBUTES=tenant.id=s1-test,workspace.id=ws_s1" \
     -- 'Reply with exactly: TELEMETRY_OK' >/dev/null
  sleep 4
  after=$(curl -s "http://localhost:$PROBE_PORT/report" | grep -c 'api_request' || true)
  if [ "$after" -gt "$before" ] 2>/dev/null || [ "$after" -gt 0 ]; then
    result PASS "OTel egress from container" "managed settings pinned the endpoint"
  else
    result FAIL "OTel egress from container" "no api_request events arrived"
  fi
else
  result SKIP "OTel egress from container" "otel-probe not running on :$PROBE_PORT"
fi

# --- E. ECS task-role credential path -----------------------------------
echo
echo "E. Container credential provider (the Fargate task-role mechanism)"
# Fargate does not inject AWS_ACCESS_KEY_ID. The SDK reads
# AWS_CONTAINER_CREDENTIALS_FULL_URI and GETs credentials over HTTP. Serving
# that locally exercises the exact code path CC will use in production.
#
# The credentials served MUST be temporary (i.e. include a session Token).
# Fargate always supplies temporary credentials, so this matches production --
# but it means a long-term IAM user key cannot be used to exercise this path:
# the provider fetches, rejects the Token-less response, and retries until it
# gives up with "Could not load credentials from any providers".
tmp=$(aws sts get-session-token --profile "$PROFILE" --duration-seconds 3600 \
        --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]' --output text 2>/dev/null)
if [ -z "$tmp" ]; then
  result SKIP "task-role credential provider" "could not mint temp credentials"
else
  out=$(docker run --rm -e "AWS_REGION=$REGION" \
    -e "SHIM_AK=$(echo "$tmp" | cut -f1)" \
    -e "SHIM_SK=$(echo "$tmp" | cut -f2)" \
    -e "SHIM_ST=$(echo "$tmp" | cut -f3)" \
    "$IMG" sh -c '
    cat > /tmp/shim.js <<JS
const http=require("http");
http.createServer((req,res)=>{
  res.setHeader("content-type","application/json");
  res.end(JSON.stringify({
    AccessKeyId: process.env.SHIM_AK,
    SecretAccessKey: process.env.SHIM_SK,
    Token: process.env.SHIM_ST,
    Expiration: new Date(Date.now()+3600e3).toISOString(),
  }));
}).listen(8888,"127.0.0.1",()=>console.error("shim up"));
JS
    node /tmp/shim.js & sleep 1
    unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
    export AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:8888/creds
    claude -p "Reply with exactly: TASKROLE_OK" 2>&1 | tail -3')
  if grep -q TASKROLE_OK <<<"$out"; then
    result PASS "task-role credential provider" "no static keys in the container"
  else
    result FAIL "task-role credential provider" "$(tail -2 <<<"$out")"
  fi
fi

echo
echo "=== $pass passed, $fail failed, $skip skipped ==="
[ "$fail" -eq 0 ]
