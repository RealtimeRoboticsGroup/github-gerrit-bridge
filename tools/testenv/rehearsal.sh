#!/bin/bash
# Production rehearsal: drives the test instance (docs/test-instance.md)
# through every flow the bridge supports, as real users on both sides, and
# checks the result of each step. Run it after any change to the bridge:
#
#   tools/testenv/rehearsal.sh
#
# People: AustinSchuh (trusted, admin) works in ~/local/bridge-play;
# AustinSchuhBRT (not trusted) in ~/local/bridge-play-brt. The bridge acts
# as realtimeroboticsbot. Every run uses fresh file names and subjects.
set -uo pipefail

R=AustinSchuh/bridge-scratch
G=http://127.0.0.1:18080/gerrit
CREDS=${HOME}/tmp/gerrit-test/bridge
ADMIN="admin:$(cat "${HOME}/tmp/gerrit-test/etc/testgerrit-admin-password")"
ME_CLONE=${HOME}/local/bridge-play
BRT_CLONE=${HOME}/local/bridge-play-brt
GH=${GH:-${HOME}/bin/gh}
RUN=$(date +%H%M%S)
TIMEOUT=${TIMEOUT:-150}

PASSED=0
FAILED=0
pass() { echo "  PASS $*"; PASSED=$((PASSED + 1)); }
fail() { echo "  FAIL $*"; FAILED=$((FAILED + 1)); }
step() { echo; echo "== $*"; }

# gh as each person.
me() { "${GH}" "$@"; }
brt() { GH_TOKEN="$("${GH}" auth token --user AustinSchuhBRT)" "${GH}" "$@"; }
# Gerrit REST: gerrit GET <path>, or gerrit_as <user> <method> <path> [json].
gerrit() { curl -sf -u "${ADMIN}" "${G}/a/$1" | tail -c +5; }
gerrit_as() {
  curl -sf -u "$1:$(cat "${CREDS}/$1.http-password")" -H 'Content-Type: application/json' \
    -X "$2" "${G}/a/$3" ${4:+-d "$4"} | tail -c +5
}

# wait_for <description> <command...>: retry until the command succeeds.
wait_for() {
  local what="$1"; shift
  local deadline=$((SECONDS + TIMEOUT))
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS > deadline)); then fail "${what} (timed out)"; return 1; fi
    sleep 3
  done
  pass "${what}"
}

# change <subject substring>: the Gerrit change number.
change() { gerrit "changes/?q=project:bridge-scratch+message:%22$(printf %s "$1" | sed 's/ /+/g')%22" | jq -r '.[0]._number // empty'; }
# pr_for <subject>: the open PR with that title.
pr_for() { me api "repos/${R}/pulls?state=all&per_page=50" --jq ".[] | select(.title==\"$1\") | .number" | head -1; }
status_is() {  # status_is <pr> <context> <state>
  local sha; sha=$(me api "repos/${R}/pulls/$1" --jq .head.sha)
  [[ "$(me api "repos/${R}/commits/${sha}/status" --jq ".statuses[] | select(.context==\"$2\") | .state")" == "$3" ]]
}
# checks <change>: the bridge's Checks-tab runs for its current patch set.
BRIDGE=http://127.0.0.1:18081
checks() { curl -sf "${BRIDGE}/checks/$1/$(gerrit "changes/$1?o=CURRENT_REVISION" | jq -r '.revisions[.current_revision]._number')"; }
commit() {  # commit <clone> <file> <subject>
  (cd "$1" && echo "$3" > "$2" && git add "$2" && git commit -qs -m "$3")
}

echo "Rehearsal ${RUN}"

# --- 1. A chain uploaded to Gerrit becomes a GitHub stack, built by CI ---
step "1. Gerrit-born chain by AustinSchuh"
S1="Chain ${RUN} part 1"; S2="Chain ${RUN} part 2"; S3="Chain ${RUN} part 3"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "chain-${RUN}" gerrit/main)
commit "${ME_CLONE}" "chain-${RUN}-1.txt" "${S1}"
commit "${ME_CLONE}" "chain-${RUN}-2.txt" "${S2}"
commit "${ME_CLONE}" "chain-${RUN}-3.txt" "${S3}"
(cd "${ME_CLONE}" && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
stacked() {
  local a b c; a=$(pr_for "${S1}"); b=$(pr_for "${S2}"); c=$(pr_for "${S3}")
  [[ -n "$c" ]] && [[ "$(me api "repos/${R}/pulls/$c" --jq '.stack.size // 0')" == 3 ]] &&
    [[ "$(me api "repos/${R}/pulls/$a" --jq .user.login)" == realtimeroboticsbot ]]
}
wait_for "3 PRs by the bot, in one native stack" stacked
P1=$(pr_for "${S1}"); P2=$(pr_for "${S2}"); P3=$(pr_for "${S3}")
C1=$(change "${S1}"); C2=$(change "${S2}"); C3=$(change "${S3}")
echo "  (changes ${C1} ${C2} ${C3} = PRs #${P1} #${P2} #${P3})"
verified_all() { for p in "$@"; do status_is "$p" gerrit/Verified success || return 1; done; }
wait_for "CI verified all three (gerrit/Verified success)" verified_all "${P1}" "${P2}" "${P3}"

# --- 2. Review comments both ways, as the people who wrote them ---
step "2. Comments, replies, suggestions and resolution"
gerrit_as AustinSchuh POST "changes/${C2}/revisions/current/review" \
  "{\"comments\":{\"chain-${RUN}-2.txt\":[{\"line\":1,\"message\":\"Rehearsal ${RUN}: tighten this line?\",\"unresolved\":true}]}}" >/dev/null
ghc() { me api "repos/${R}/pulls/${P2}/comments" --jq ".[] | select(.body | contains(\"Rehearsal ${RUN}: tighten\")) | $1" | head -1; }
by_bot() { [[ "$(ghc .user.login)" == realtimeroboticsbot ]]; }
wait_for "Gerrit comment appears on GitHub, posted by the bot" by_bot
ROOT=$(ghc .id)
# Anyone with write access can edit the bot's comments; Gerrit's stay Gerrit's.
me api -X PATCH "repos/${R}/pulls/comments/${ROOT}" -f body="Rehearsal ${RUN}: rewritten on GitHub" --silent
restored() {
  local b; b=$(me api "repos/${R}/pulls/comments/${ROOT}" --jq .body)
  [[ "$b" == *"Restored by the bridge"* && "$b" == *"tighten this line"* && "$b" != *"rewritten on GitHub"* ]]
}
wait_for "an edit on GitHub to the bot's copy of a Gerrit comment is undone" restored
brt api "repos/${R}/pulls/${P2}/comments/${ROOT}/replies" -f body="Rehearsal ${RUN}: done, thanks." --silent
by_brt() { gerrit "changes/${C2}/comments" | jq -e ".[][] | select(.message == \"Rehearsal ${RUN}: done, thanks.\") | select(.author.username == \"AustinSchuhBRT\")"; }
wait_for "GitHub reply lands in the Gerrit thread as AustinSchuhBRT (Run-As), without an attribution header" by_brt
HEAD2=$(me api "repos/${R}/pulls/${P2}" --jq .head.sha)
brt api "repos/${R}/pulls/${P2}/comments" -f commit_id="${HEAD2}" -f path="chain-${RUN}-2.txt" -F line=1 -f side=RIGHT \
  -f body=$'Rehearsal '"${RUN}"$': suggestion\n```suggestion\nSuggested by BRT\n```' --silent
fix() { gerrit "changes/${C2}/comments" | jq -e ".[][] | select(.message | contains(\"Rehearsal ${RUN}: suggestion\")) | select(.fix_suggestions | length > 0)"; }
wait_for "GitHub suggestion becomes a Gerrit fix suggestion" fix
# BRT resolves both threads (the suggestion's too, or AOS's
# No-Unresolved-Comments would rightly block the submit in step 4).
for TID in $(me api graphql -f query="query{repository(owner:\"AustinSchuh\",name:\"bridge-scratch\"){pullRequest(number:${P2}){reviewThreads(first:50){nodes{id isResolved}}}}}" \
  --jq '.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved | not) | .id'); do
  brt api graphql -f query="mutation{resolveReviewThread(input:{threadId:\"${TID}\"}){thread{isResolved}}}" --silent
done
resolved() { gerrit "changes/${C2}/comments" | jq -e "[.[][] | select(.message == \"Resolved on GitHub.\") | select(.author.username == \"AustinSchuhBRT\")] | length == 2"; }
wait_for "resolving on GitHub resolves both Gerrit threads, as the resolver" resolved
no_unresolved() { gerrit "changes/${C2}?o=SUBMIT_REQUIREMENTS" | jq -e '[.submit_requirements[] | select(.name == "No-Unresolved-Comments" and .status == "UNSATISFIED")] | length == 0'; }
wait_for "nothing left unresolved on change ${C2}" no_unresolved

# --- 3. Partial push and restack (design §5.4, rule 2) ---
step "3. Amend only the middle change, then rebase the top"
(cd "${ME_CLONE}" && git checkout -q HEAD~1 && echo "amended" >> "chain-${RUN}-2.txt" && git commit -qa --amend --no-edit &&
  git push -q gerrit HEAD:refs/for/main 2>/dev/null)
frozen() { [[ "$(me api "repos/${R}/pulls/${P3}" --jq .base.ref)" == gerrit-base/* ]]; }
wait_for "the top PR moves to a frozen base and leaves the stack" frozen
# head_commit <pr> <jq>: a field of the PR head's commit.
head_commit() { me api "repos/${R}/commits/$(me api "repos/${R}/pulls/$1" --jq .head.sha)" --jq "$2"; }
appended() { [[ "$(head_commit "${P2}" '[.parents[].sha] | join(" ")')" == "${HEAD2}" ]] &&
  [[ "$(head_commit "${P2}" '.commit.message')" == "Patch set 2 from Gerrit"* ]]; }
wait_for "the amend is a new commit on top of patch set 1 (history kept)" appended
outdated_only() {  # the suggestion on line 1 is outdated (the line changed); the thread is still there
  me api graphql -f query="query{repository(owner:\"AustinSchuh\",name:\"bridge-scratch\"){pullRequest(number:${P2}){reviewThreads(first:50){nodes{comments(first:1){nodes{body}}}}}}}" \
    --jq '.data.repository.pullRequest.reviewThreads.nodes[].comments.nodes[].body' | grep -q "Rehearsal ${RUN}: suggestion"
}
wait_for "the review threads stay on the PR" outdated_only
(cd "${ME_CLONE}" && git cherry-pick "chain-${RUN}" >/dev/null && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
rejoined() { [[ "$(me api "repos/${R}/pulls/${P3}" --jq '.stack.size // 0')" == 3 ]] && [[ "$(me api "repos/${R}/pulls/${P3}" --jq .base.ref)" == gerrit/* ]]; }
wait_for "rebasing it rejoins the stack" rejoined
merge_of_base() { [[ "$(head_commit "${P3}" '.parents | length')" == 2 ]] &&
  [[ "$(head_commit "${P3}" '.commit.message')" == "Patch set 2 from Gerrit, rebased"* ]] &&
  [[ "$(me api "repos/${R}/pulls/${P3}/files" --jq '[.[].filename] | join(" ")')" == "chain-${RUN}-3.txt" ]]; }
wait_for "the rebase is a merge of the new base; the PR still shows only its own file" merge_of_base

# --- 4. Approve on GitHub, /submit from GitHub, replication marks merged ---
step "4. Approve on GitHub, then /submit"
for p in "${P1}" "${P2}" "${P3}"; do me api "repos/${R}/pulls/${p}/reviews" -f event=APPROVE -f body="Rehearsal ${RUN}: LGTM" --silent; done
cr2() { for c in "${C1}" "${C2}" "${C3}"; do gerrit "changes/${c}?o=DETAILED_LABELS&o=DETAILED_ACCOUNTS" | jq -e '.labels["Code-Review"].all[] | select(.username == "AustinSchuh" and .value == 2)' || return 1; done; }
wait_for "GitHub approvals become Code-Review +2 by AustinSchuh" cr2
submittable() { for c in "${C1}" "${C2}" "${C3}"; do gerrit "changes/${c}?o=SUBMITTABLE" | jq -e '.submittable' || return 1; done; }
wait_for "all three submittable (CI re-verified the new patch sets)" submittable
me api "repos/${R}/issues/${P3}/comments" -f body="/submit" --silent
merged() { for p in "${P1}" "${P2}" "${P3}"; do [[ "$(me api "repos/${R}/pulls/${p}" --jq .merged)" == true ]] || return 1; done; }
wait_for "/submit merges the chain; replication makes GitHub show all three merged" merged

# --- 5. A stack opened on GitHub by the untrusted contributor ---
step "5. GitHub-born stack by AustinSchuhBRT"
B1="brt-${RUN}-1"; B2="brt-${RUN}-2"; T1="BRT stack ${RUN} layer 1"; T2="BRT stack ${RUN} layer 2"
(cd "${BRT_CLONE}" && git fetch -q github && git checkout -q -B "${B1}" github/main)
commit "${BRT_CLONE}" "brt-${RUN}-1.txt" "${T1}"
(cd "${BRT_CLONE}" && git checkout -q -B "${B2}")
commit "${BRT_CLONE}" "brt-${RUN}-2.txt" "${T2}"
(cd "${BRT_CLONE}" && git push -q github "${B1}" "${B2}" 2>/dev/null)
Q1=$(brt api "repos/${R}/pulls" -f title="${T1}" -f head="${B1}" -f base=main -f body="Rehearsal." --jq .number)
Q2=$(brt api "repos/${R}/pulls" -f title="${T2}" -f head="${B2}" -f base="${B1}" -f body="Rehearsal." --jq .number)
brt api "repos/${R}/stacks" --input - --silent <<<"{\"pull_requests\":[${Q1},${Q2}]}"
owned_by_brt() { [[ "$(gerrit "changes/$(change "${T2}")?o=DETAILED_ACCOUNTS" | jq -r .owner.username)" == AustinSchuhBRT ]]; }
wait_for "both layers imported, owned by AustinSchuhBRT" owned_by_brt
D1=$(change "${T1}"); D2=$(change "${T2}")
wait_for "gerrit/sync reports success" status_is "${Q1}" gerrit/sync success
wait_for "CI did not start for the untrusted uploader" status_is "${Q1}" gerrit/Verified pending
checks "${D1}" | jq -e '.runs == []' >/dev/null && pass "the Checks tab shows no runs" || fail "Checks tab: $(checks "${D1}")"

step "6. retest from GitHub by a trusted reviewer"
me api "repos/${R}/issues/${Q1}/comments" -f body="retest" --silent
wait_for "AustinSchuh's retest on GitHub starts CI (gerrit/Verified success)" status_is "${Q1}" gerrit/Verified success
checks "${D1}" | jq -e '[.runs[].checkName] | sort == ["Build","Test"]' >/dev/null && pass "the Checks tab shows the retest's jobs" || fail "Checks tab: $(checks "${D1}")"

step "7. AustinSchuh pushes to BRT's branch"
(cd "${ME_CLONE}" && git fetch -q github && git checkout -q -B "${B2}" "github/${B2}" && echo "from AustinSchuh" >> "brt-${RUN}-2.txt" &&
  git commit -qas -m "More" && git push -q github "${B2}" 2>/dev/null)
pusher() { gerrit "changes/${D2}?o=CURRENT_REVISION&o=DETAILED_ACCOUNTS" | jq -e '.revisions[.current_revision] | select(._number == 2 and .uploader.username == "AustinSchuh")'; }
wait_for "the new patch set is uploaded as AustinSchuh (the pusher)" pusher
[[ "$(gerrit "changes/${D2}?o=DETAILED_ACCOUNTS" | jq -r .owner.username)" == AustinSchuhBRT ]] && pass "the change stays owned by AustinSchuhBRT" || fail "owner changed"

step "8. Draft toggles"
NODE=$(me api "repos/${R}/pulls/${Q1}" --jq .node_id)
brt api graphql -f query="mutation{convertPullRequestToDraft(input:{pullRequestId:\"${NODE}\"}){pullRequest{isDraft}}}" --silent
wip() { [[ "$(gerrit "changes/${D1}" | jq -r '.work_in_progress // false')" == "$1" ]]; }
wait_for "draft on GitHub makes the change WIP" wip true
brt api graphql -f query="mutation{markPullRequestReadyForReview(input:{pullRequestId:\"${NODE}\"}){pullRequest{isDraft}}}" --silent
wait_for "ready for review clears WIP" wip false

# --- 9. Submit in Gerrit: the bridge must retarget the PRs to main before
# replication (replicationDelay, 15s as on AOS) pushes the merge ---
step "9. A chain submitted in Gerrit"
E1="Gerrit submit ${RUN} part 1"; E2="Gerrit submit ${RUN} part 2"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "gsubmit-${RUN}" gerrit/main)
commit "${ME_CLONE}" "gsubmit-${RUN}-1.txt" "${E1}"
commit "${ME_CLONE}" "gsubmit-${RUN}-2.txt" "${E2}"
(cd "${ME_CLONE}" && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
two_prs() { [[ -n "$(pr_for "${E1}")" && -n "$(pr_for "${E2}")" ]]; }
wait_for "2 PRs opened" two_prs
F1=$(change "${E1}"); F2=$(change "${E2}"); O1=$(pr_for "${E1}"); O2=$(pr_for "${E2}")
for c in "${F1}" "${F2}"; do gerrit_as AustinSchuh POST "changes/${c}/revisions/current/review" '{"labels":{"Code-Review":2}}' >/dev/null; done
gsubmittable() { for c in "${F1}" "${F2}"; do gerrit "changes/${c}?o=SUBMITTABLE" | jq -e '.submittable' || return 1; done; }
wait_for "both submittable once CI verifies" gsubmittable
# Amend the top one, so its PR head is the bridge's history commit: the
# bridge must swap in Gerrit's commit before replication lands it.
(cd "${ME_CLONE}" && echo "amended" >> "gsubmit-${RUN}-2.txt" && git commit -qa --amend --no-edit && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
ps2_on_github() { [[ "$(head_commit "${O2}" '.commit.message')" == "Patch set 2 from Gerrit"* ]]; }
wait_for "the amend reaches the PR as a new commit" ps2_on_github
for c in "${F1}" "${F2}"; do gerrit_as AustinSchuh POST "changes/${c}/revisions/current/review" '{"labels":{"Code-Review":2}}' >/dev/null; done
wait_for "both submittable again once CI verifies" gsubmittable
gerrit_as AustinSchuh POST "changes/${F2}/submit" "{}" >/dev/null
gmerged() { for p in "${O1}" "${O2}"; do [[ "$(me api "repos/${R}/pulls/${p}" --jq .merged)" == true ]] || return 1; done; }
wait_for "GitHub shows both PRs merged" gmerged

# --- 10. A PR from a fork, on a branch named like the repository's main ---
step "10. A PR from AustinSchuhBRT's fork"
FORK=AustinSchuhBRT/bridge-scratch
K="Fork ${RUN}"
(cd "${BRT_CLONE}" && { git remote get-url fork >/dev/null 2>&1 || git remote add fork "https://github.com/${FORK}.git"; } &&
  git fetch -q github && git checkout -q -B "fork-${RUN}" github/main)
commit "${BRT_CLONE}" "fork-${RUN}.txt" "${K}"
(cd "${BRT_CLONE}" && git push -qf fork HEAD:refs/heads/main 2>/dev/null)
brt api "repos/${R}/pulls" -f title="${K}" -f head="AustinSchuhBRT:main" -f base=main -f body="From a fork." -F maintainer_can_modify=true --silent
FQ=$(pr_for "${K}")
fork_imported() { [[ "$(gerrit "changes/?q=project:bridge-scratch+message:%22Fork+${RUN}%22&o=DETAILED_ACCOUNTS" | jq -r '.[0].owner.username')" == AustinSchuhBRT ]]; }
wait_for "imported, owned by AustinSchuhBRT" fork_imported
FK=$(change "${K}")
wait_for "gerrit/sync reports success" status_is "${FQ}" gerrit/sync success
# A new patch set in Gerrit, by AustinSchuh.
gerrit_ps() {  # gerrit_ps <file>: amend change FK with a new file and push
  (cd "${ME_CLONE}" && git fetch -q gerrit "$(gerrit "changes/${FK}?o=CURRENT_REVISION" | jq -r '.revisions[.current_revision].ref')" &&
    git checkout -q FETCH_HEAD && echo "$1" > "$1" && git add "$1" && git commit -q --amend --no-edit && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
}
gerrit_ps "fork-${RUN}-gerrit.txt"
on_fork() { me api "repos/${FORK}/contents/$1?ref=main" --silent 2>/dev/null; }
wait_for "a Gerrit patch set is pushed to the fork's branch (maintainer edits allowed)" on_fork "fork-${RUN}-gerrit.txt"
main_untouched() { [[ "$(me api "repos/${R}/contents/fork-${RUN}-gerrit.txt?ref=main" 2>&1)" == *"Not Found"* ]]; }
main_untouched && pass "the repository's own main is untouched" || fail "a fork edit reached the repository's main"
# Now without maintainer edits.
brt api -X PATCH "repos/${R}/pulls/${FQ}" -F maintainer_can_modify=false --silent
gerrit_ps "fork-${RUN}-locked.txt"
sync_says() { local sha; sha=$(me api "repos/${R}/pulls/${FQ}" --jq .head.sha)
  me api "repos/${R}/commits/${sha}/status" --jq '.statuses[] | select(.context=="gerrit/sync") | .description' | grep -q "$1"; }
wait_for "without maintainer edits, gerrit/sync says the newer patch set is only in Gerrit" sync_says "only in Gerrit"
on_fork "fork-${RUN}-locked.txt" && fail "pushed to a fork that does not allow it" || pass "nothing pushed to the fork"
# Abandoning in Gerrit closes the PR (and frees the fork's main for the next run).
gerrit_as AustinSchuhBRT POST "changes/${FK}/abandon" "{}" >/dev/null
fork_closed() { [[ "$(me api "repos/${R}/pulls/${FQ}" --jq .state)" == closed ]]; }
wait_for "abandoning in Gerrit closes the fork PR" fork_closed

# --- 11. CI in Gerrit's Checks tab (plugins/bridge-checks.js) ---
step "11. CI results in Gerrit's Checks tab, and on GitHub"
gerrit "plugins/" | jq -e '.["bridge-checks"]' >/dev/null && pass "Gerrit runs the bridge-checks plugin" || fail "bridge-checks plugin not loaded"
[[ "$(curl -s -o /dev/null -D - -H 'Origin: http://127.0.0.1:18080' "${BRIDGE}/checks/1/1" | tr -d '\r' | sed -n 's/^Access-Control-Allow-Origin: //Ip')" == http://127.0.0.1:18080 ]] &&
  pass "the bridge lets Gerrit's pages read /checks (CORS)" || fail "no CORS header for Gerrit's origin"
S11="Checks ${RUN}"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "checks-${RUN}" gerrit/main)
commit "${ME_CLONE}" "checks-${RUN}-FAIL.txt" "${S11}"
(cd "${ME_CLONE}" && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
has_pr() { [[ -n "$(pr_for "${S11}")" ]]; }
wait_for "the change has a PR" has_pr
E1=$(change "${S11}"); PE=$(pr_for "${S11}")
failed_job() {
  checks "${E1}" | jq -e '[.runs[] | select(.attempt == 1 and .status == "COMPLETED") | "\(.checkName) \(.results[0].category) \(.results[0].summary)"] | sort ==
    ["Build SUCCESS Passed", "Test ERROR Failed (exit status 1)"]'
}
wait_for "each job's result: Build passed, Test failed (exit status 1)" failed_job
vfail() {
  local sha; sha=$(me api "repos/${R}/pulls/${PE}" --jq .head.sha)
  me api "repos/${R}/commits/${sha}/status" --jq '.statuses[] | select(.context=="gerrit/Verified") | "\(.state) \(.target_url)"' |
    grep -q '^failure http://127.0.0.1:18090/builds/'
}
wait_for "GitHub's gerrit/Verified fails, linking the Buildkite build" vfail
# What the plugin's Rerun button does: "retest" as the signed-in user.
gerrit_as AustinSchuh POST "changes/${E1}/revisions/current/review" '{"message":"retest"}' >/dev/null
attempt2() { checks "${E1}" | jq -e '[.runs[] | select(.attempt == 2)] | length == 2'; }
wait_for "Rerun (retest) adds a second attempt" attempt2
gerrit_as AustinSchuh POST "changes/${E1}/abandon" "{}" >/dev/null
closed() { [[ "$(me api "repos/${R}/pulls/${PE}" --jq .state)" == closed ]]; }
wait_for "abandoning closes the PR" closed

# --- 12. Someone restacks a Gerrit-born stack on GitHub (design §5.2.2):
# what GitHub's "Rebase stack" button does, pushed as AustinSchuhBRT ---
step "12. AustinSchuhBRT rebases AustinSchuh's Gerrit stack on GitHub"
U1="Restack ${RUN} part 1"; U2="Restack ${RUN} part 2"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "restack-${RUN}" gerrit/main~1)
commit "${ME_CLONE}" "restack-${RUN}-1.txt" "${U1}"
commit "${ME_CLONE}" "restack-${RUN}-2.txt" "${U2}"
(cd "${ME_CLONE}" && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
two_restack() { [[ -n "$(pr_for "${U1}")" && -n "$(pr_for "${U2}")" ]] && [[ "$(me api "repos/${R}/pulls/$(pr_for "${U2}")" --jq '.stack.size // 0')" == 2 ]]; }
wait_for "a stack of 2 PRs on an older main" two_restack
H1=$(change "${U1}"); H2=$(change "${U2}"); V1=$(pr_for "${U1}"); V2=$(pr_for "${U2}")
CID1=$(gerrit "changes/${H1}" | jq -r .change_id); CID2=$(gerrit "changes/${H2}" | jq -r .change_id)
(cd "${BRT_CLONE}" && git fetch -q github && git checkout -q -B "restack-${RUN}" "github/gerrit/${CID2}" &&
  git rebase -q github/main 2>/dev/null &&
  git push -q --force github "HEAD~1:refs/heads/gerrit/${CID1}" "HEAD:refs/heads/gerrit/${CID2}" 2>/dev/null)
RB1=$(cd "${BRT_CLONE}" && git rev-parse HEAD~1); RB2=$(cd "${BRT_CLONE}" && git rev-parse HEAD)
restacked() {
  local a b; a=$(gerrit "changes/${H1}?o=CURRENT_REVISION&o=DETAILED_ACCOUNTS&o=CURRENT_COMMIT"); b=$(gerrit "changes/${H2}?o=CURRENT_REVISION&o=DETAILED_ACCOUNTS&o=CURRENT_COMMIT")
  jq -e '.revisions[.current_revision] | select(._number == 2 and .uploader.username == "AustinSchuhBRT")' <<<"$a" &&
    jq -e '.revisions[.current_revision] | select(._number == 2 and .uploader.username == "AustinSchuhBRT")' <<<"$b" &&
    [[ "$(jq -r '.revisions[.current_revision].commit.parents[0].commit' <<<"$b")" == "$(jq -r .current_revision <<<"$a")" ]] &&
    [[ "$(jq -r '.revisions[.current_revision].commit.parents[0].commit' <<<"$a")" == "$(gerrit "projects/bridge-scratch/branches/main" | jq -r .revision)" ]]
}
wait_for "both layers become patch set 2, uploaded by the pusher, chained on the current main" restacked
[[ "$(gerrit "changes/${H1}?o=DETAILED_ACCOUNTS" | jq -r .owner.username)" == AustinSchuh ]] && pass "the change stays AustinSchuh's" || fail "owner changed"
kept() { [[ "$(me api "repos/${R}/pulls/${V1}" --jq .head.sha)" == "${RB1}" && "$(me api "repos/${R}/pulls/${V2}" --jq .head.sha)" == "${RB2}" ]]; }
sleep 10 # give the bridge a chance to (wrongly) push over the rebase
kept && pass "the bridge leaves the rebased branches as pushed" || fail "the rebase was overwritten"
wait_for "gerrit/sync says the top layer is in sync" status_is "${V2}" gerrit/sync success
for c in "${H2}" "${H1}"; do gerrit_as AustinSchuh POST "changes/${c}/abandon" "{}" >/dev/null; done

# --- 13. Closing and reopening on GitHub abandons and restores in Gerrit,
# as the person who did it; Gerrit's permissions decide (design §6.6) ---
step "13. Close and reopen on GitHub"
W1="Close ${RUN}"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "close-${RUN}" gerrit/main)
commit "${ME_CLONE}" "close-${RUN}.txt" "${W1}"
(cd "${ME_CLONE}" && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
has_close_pr() { [[ -n "$(pr_for "${W1}")" ]]; }
wait_for "the change has a PR" has_close_pr
K1=$(change "${W1}"); X1=$(pr_for "${W1}")
gstatus() { [[ "$(gerrit "changes/${K1}" | jq -r .status)" == "$1" ]]; }
prstate() { [[ "$(me api "repos/${R}/pulls/${X1}" --jq .state)" == "$1" ]]; }
brt api -X PATCH "repos/${R}/pulls/${X1}" -f state=closed --silent
refused() { prstate open && gstatus NEW && me api "repos/${R}/issues/${X1}/comments" --jq '.[].body' | grep -q "would not let AustinSchuhBRT abandon it"; }
wait_for "AustinSchuhBRT may not abandon AustinSchuh's change: the PR is reopened, saying why" refused
me api -X PATCH "repos/${R}/pulls/${X1}" -f state=closed --silent
abandoned() { gstatus ABANDONED && [[ "$(gerrit "changes/${K1}/detail?o=DETAILED_ACCOUNTS" | jq -r '.messages[-1].author.username')" == AustinSchuh ]]; }
wait_for "AustinSchuh closing it abandons the change, as AustinSchuh" abandoned
sleep 8
prstate closed && pass "the PR stays closed" || fail "the PR was reopened"
# Restored in Gerrit: the old GitHub close must not abandon it again.
gerrit_as AustinSchuh POST "changes/${K1}/restore" "{}" >/dev/null
wait_for "restoring in Gerrit reopens the PR" prstate open
sleep 8
gstatus NEW && pass "and the change stays restored" || fail "the old GitHub close abandoned it again"
me api -X PATCH "repos/${R}/pulls/${X1}" -f state=closed --silent
wait_for "closing it again abandons it again" gstatus ABANDONED
me api -X PATCH "repos/${R}/pulls/${X1}" -f state=open --silent
restored() { gstatus NEW && prstate open; }
wait_for "reopening it restores the change" restored
gerrit_as AustinSchuh POST "changes/${K1}/abandon" "{}" >/dev/null

# --- 14. PR edits on GitHub reach Gerrit, wherever the change started
# (design §6.0): the title and description become the commit message, and
# draft becomes WIP, as the person who did it ---
step "14. Edit a Gerrit change's PR on GitHub"
Y1="Edit ${RUN}"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "edit-${RUN}" gerrit/main)
commit "${ME_CLONE}" "edit-${RUN}.txt" "${Y1}"
(cd "${ME_CLONE}" && git push -q gerrit HEAD:refs/for/main 2>/dev/null)
has_edit_pr() { [[ -n "$(pr_for "${Y1}")" ]]; }
wait_for "the change has a PR" has_edit_pr
L1=$(change "${Y1}"); Z1=$(pr_for "${Y1}")
sleep 10 # let the bridge record the PR and change as in sync
BODY="$(me api "repos/${R}/pulls/${Z1}" --jq .body)"
me api -X PATCH "repos/${R}/pulls/${Z1}" -f title="${Y1}, retitled on GitHub" -f body="Described on GitHub.

${BODY}" --silent
retitled() {
  local c; c=$(gerrit "changes/${L1}?o=CURRENT_REVISION&o=CURRENT_COMMIT&o=DETAILED_ACCOUNTS")
  [[ "$(jq -r '.revisions[.current_revision].commit.message' <<<"$c")" == "${Y1}, retitled on GitHub"$'\n\n'"Described on GitHub."* ]] &&
    jq -e '.revisions[.current_revision] | select(._number == 2 and .uploader.username == "AustinSchuh")' <<<"$c"
}
wait_for "the new title and description become patch set 2's commit message, by AustinSchuh" retitled
gerrit "changes/${L1}?o=CURRENT_REVISION&o=CURRENT_COMMIT" | jq -e '.revisions[.current_revision].commit.message | contains("Change-Id: ") and contains("Signed-off-by: ")' >/dev/null &&
  pass "the Change-Id and sign-off are kept" || fail "trailers lost"
sleep 8
[[ "$(me api "repos/${R}/pulls/${Z1}" --jq .title)" == "${Y1}, retitled on GitHub" ]] && pass "the PR keeps the new title" || fail "the title was put back"
NODE=$(me api "repos/${R}/pulls/${Z1}" --jq .node_id)
me api graphql -f query="mutation{convertPullRequestToDraft(input:{pullRequestId:\"${NODE}\"}){pullRequest{isDraft}}}" --silent
wipped() { [[ "$(gerrit "changes/${L1}" | jq -r '.work_in_progress // false')" == true ]]; }
wait_for "draft on GitHub makes the Gerrit-born change WIP" wipped
gerrit_as AustinSchuh POST "changes/${L1}/ready" "{}" >/dev/null
not_draft() { [[ "$(me api "repos/${R}/pulls/${Z1}" --jq .draft)" == false ]]; }
wait_for "ready in Gerrit makes the PR ready" not_draft
gerrit_as AustinSchuh POST "changes/${L1}/abandon" "{}" >/dev/null

echo
echo "Rehearsal ${RUN}: ${PASSED} passed, ${FAILED} failed"
((FAILED == 0))
