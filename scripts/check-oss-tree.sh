#!/usr/bin/env bash
# Fail if draft, spike, probe, or lab-note paths are still git-tracked.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

forbidden=(
	'CC_REMOTE_ANALYSIS.md'
	'NEXT_SESSION_PROMPT.md'
	'litellm-spike/'
	'otel-probe/'
	'winch-probe/'
	's1-bedrock/'
	'tui-proxy-proto/'
	'RESULTS.md'
	'MANUAL_MATRIX.md'
)

tracked="$(git ls-files)"
fail=0
for path in "${forbidden[@]}"; do
	if printf '%s\n' "$tracked" | grep -F -q -- "$path"; then
		echo "forbidden path still tracked: $path" >&2
		fail=1
	fi
done

if [[ $fail -ne 0 ]]; then
	exit 1
fi

echo "oss tree is clean of draft/spike/probe/results paths"
