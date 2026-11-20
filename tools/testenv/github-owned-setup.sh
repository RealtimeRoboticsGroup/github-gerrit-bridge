#!/bin/bash
# Sets up the GitHub side of the test instance's GitHub-owned project
# (design §3.1, setup checklist step 1): a public repository (rulesets need
# a paid plan on private ones), where GitHub merges and Gerrit follows.
# Idempotent; install.sh runs it. As AustinSchuh (gh's active account);
# AustinSchuhBRT accepts its own invitation and adds the workflow (gh's
# AustinSchuh token has no workflow scope).
#
# Usage: github-owned-setup.sh <gh> <owner/repo> <bot> <second>
set -euo pipefail
GH="$1" REPO="$2" BOT="$3" SECOND="$4"
me() { "${GH}" "$@"; }
second() { GH_TOKEN="$("${GH}" auth token --user "${SECOND}")" "${GH}" "$@"; }

if ! me api "repos/${REPO}" --silent 2>/dev/null; then
  me repo create "${REPO}" --public --add-readme \
    --description "Scratch repository for the bridge's test instance: GitHub owns main, Gerrit follows"
fi
# Merges take the PR's description, which ends with its Change-Id.
me api -X PATCH "repos/${REPO}" -F allow_squash_merge=true -F allow_merge_commit=true -F allow_rebase_merge=false \
  -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY \
  -f merge_commit_title=PR_TITLE -f merge_commit_message=PR_BODY -F delete_branch_on_merge=false --silent

for who in "${BOT}" "${SECOND}"; do
  if ! me api "repos/${REPO}/collaborators/${who}" --silent 2>/dev/null; then
    me api -X PUT "repos/${REPO}/collaborators/${who}" -f permission=push --silent
  fi
done
for id in $(second api user/repository_invitations --jq ".[] | select(.repository.full_name == \"${REPO}\") | .id"); do
  second api -X PATCH "user/repository_invitations/${id}" --silent
done

# CI: two checks, so the Checks tab shows which one failed. "build" fails
# on any file containing CI-MUST-FAIL; "lint" always passes.
WF=.github/workflows/ci.yml
if ! me api "repos/${REPO}/contents/${WF}" --silent 2>/dev/null; then
  second api -X PUT "repos/${REPO}/contents/${WF}" -f message="CI for the bridge's test instance" \
    -f content="$(base64 -w0 <<'YAML'
name: CI
on: [pull_request, push]
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Build
        run: |
          if git grep -l CI-MUST-FAIL -- . ':!.github'; then
            echo "::error title=Build failed::a file asks CI to fail"
            exit 1
          fi
          echo built
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo linted
YAML
)" --silent
fi

# GitHub gates merges: one approval and the build. Admins (AustinSchuh)
# may bypass, so the rehearsal can force-push main and merge as it needs.
RULESET='{"name":"GitHub owns main","target":"branch","enforcement":"active",
  "conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"],"exclude":[]}},
  "bypass_actors":[{"actor_type":"RepositoryRole","actor_id":5,"bypass_mode":"always"}],
  "rules":[{"type":"deletion"},
    {"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":false,
      "require_code_owner_review":false,"require_last_push_approval":false,"required_review_thread_resolution":false}},
    {"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,
      "required_status_checks":[{"context":"build"}]}}]}'
RID="$(me api "repos/${REPO}/rulesets" --jq '.[] | select(.name == "GitHub owns main") | .id')"
if [[ -n "${RID}" ]]; then
  me api -X PUT "repos/${REPO}/rulesets/${RID}" --input - --silent <<<"${RULESET}"
else
  me api "repos/${REPO}/rulesets" --input - --silent <<<"${RULESET}"
fi

# What only the people themselves can do.
if [[ "$(me api "repos/${REPO}/collaborators/${BOT}/permission" --jq .permission 2>/dev/null)" != write &&
      -n "$(me api "repos/${REPO}/invitations" --jq ".[] | select(.invitee.login == \"${BOT}\") | .id")" ]]; then
  echo "NOTE: ${BOT} must accept its invitation to ${REPO} (https://github.com/${REPO}/invitations, signed in as ${BOT})" >&2
fi
