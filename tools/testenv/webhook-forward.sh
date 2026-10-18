#!/bin/bash
# Forwards GitHub webhooks to the bridge (the bridge-test-webhooks service).
#
# `gh webhook forward` creates a temporary "cli" webhook and deletes it on
# exit, but if its connection drops it exits without deleting it, and every
# later start then fails with "Hook already exists". Delete any left-over
# forwarder hook first; only hooks pointing at GitHub's forwarder are touched.
#
# Usage: webhook-forward.sh <gh> <repo> <events> <url> <secret file>
set -euo pipefail
GH="$1" REPO="$2" EVENTS="$3" URL="$4" SECRET_FILE="$5"

for id in $("${GH}" api "repos/${REPO}/hooks" \
    --jq '.[] | select(.name == "cli" and (.config.url // "" | startswith("https://webhook-forwarder.github.com/"))) | .id'); do
  echo "deleting stale forwarder webhook ${id}"
  "${GH}" api -X DELETE "repos/${REPO}/hooks/${id}" >/dev/null
done
exec "${GH}" webhook forward --repo="${REPO}" --events="${EVENTS}" --url="${URL}" --secret="$(cat "${SECRET_FILE}")"
