#!/usr/bin/env bash
# The kit's headline promise: importing agentkit does not drag in a cloud SDK,
# a metrics stack, or anyone's service client. This asserts it mechanically so a
# stray import cannot quietly undo the extraction.
#
# Rationale: the pre-extraction package imported an in-house LLM package and
# cost 537 transitive packages — 341 of them non-stdlib (95 aws-sdk, 60 grpc). Inverting that seam is
# whole point of this module; this script is the tripwire.
set -euo pipefail

MAX_EXTERNAL=${MAX_EXTERNAL:-5}

# Non-stdlib deps of the root package: stdlib has no dot in its first path segment.
external=$(go list -deps ./... 2>/dev/null | grep '\.' | grep -v '^github.com/mya-ai/agentkit' | grep -v '^vendor/' | sort -u || true)
count=$(printf '%s' "$external" | grep -c . || true)

echo "External (non-stdlib) dependencies: ${count} (budget: ${MAX_EXTERNAL})"
if [ -n "$external" ]; then printf '%s\n' "$external" | sed 's/^/  /'; fi

# Packages that must NEVER appear. Each one previously rode in via the LLM seam.
banned='aws-sdk-go|google.golang.org/genai|prometheus|google.golang.org/grpc|mya-monorepo|mya-shared'
if printf '%s' "$external" | grep -Eq "$banned"; then
  echo "::error::a banned dependency is reachable from agentkit:"
  printf '%s\n' "$external" | grep -E "$banned" | sed 's/^/  /'
  exit 1
fi

if [ "$count" -gt "$MAX_EXTERNAL" ]; then
  echo "::error::external dependency budget exceeded (${count} > ${MAX_EXTERNAL})."
  echo "Adding a dependency to the core kit is a design decision — raise MAX_EXTERNAL"
  echo "deliberately, or put the dependency behind an interface the consumer implements."
  exit 1
fi

echo "OK"
