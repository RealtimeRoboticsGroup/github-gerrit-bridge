#!/bin/bash
# Rehearsal of GitHub-owned branches (branch_owner: github, design §3.1):
# drives the test instance's GitHub-owned project (docs/test-instance.md)
# through each flow, as real users on both sides, and checks each result.
# GitHub merges and gates merges; Gerrit follows and mirrors.
#
#   tools/testenv/rehearsal-github.sh
#
# People: AustinSchuh (admin on both sides, linked to this project's
# bridge) works in ~/local/bridge-play-<repo>; AustinSchuhBRT (a writer on
# GitHub, not linked) in ~/local/bridge-play-<repo>-brt; bob (Gerrit only,
# not linked) pushes to Gerrit. Every run uses fresh branches and subjects.
set -uo pipefail

R=${FOLLOW_REPO:-AustinSchuh/bridge-scratch-gh2}
P=${R##*/}
G=http://127.0.0.1:18080/gerrit
FRONT=http://127.0.0.1:18079/gerrit
CREDS=${HOME}/tmp/gerrit-test/bridge
LOG=${HOME}/tmp/bridge-test/logs/bridge-test-gh-bridge.log
ME_CLONE=${HOME}/local/bridge-play-${R##*/}
BRT_CLONE=${HOME}/local/bridge-play-${R##*/}-brt
GH=${GH:-${HOME}/bin/gh}
RUN=$(date +%H%M%S)
TIMEOUT=${TIMEOUT:-240}

PASSED=0
FAILED=0
pass() { echo "  PASS $*"; PASSED=$((PASSED + 1)); }
fail() { echo "  FAIL $*"; FAILED=$((FAILED + 1)); }
step() { echo; echo "== $*"; }
# A /submit is about the PR head that had been on GitHub for a minute
# when it was written (comments.go minSeen).
read_it() { sleep 65; }

me() { "${GH}" "$@"; }
brt() { GH_TOKEN="$("${GH}" auth token --user AustinSchuhBRT)" "${GH}" "$@"; }
# Gerrit REST as AustinSchuh (an administrator there): gerrit <path>, or
# gerrit_as <user> <method> <path> [json].
gerrit_as() {
  # The password through curl's config on stdin, never its argv (ps).
  curl -sf -K- -H 'Content-Type: application/json' <<<"user = \"$1:$(cat "${CREDS}/$1.http-password")\"" \
    -X "$2" "${G}/a/$3" ${4:+-d "$4"} | tail -c +5
}
gerrit() { gerrit_as AustinSchuh GET "$1"; }

wait_for() {
  local what="$1"; shift
  local deadline=$((SECONDS + TIMEOUT))
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS > deadline)); then fail "${what} (timed out)"; return 1; fi
    sleep 3
  done
  pass "${what}"
}
check() {  # check <description> <command...>: once, now.
  local what="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "${what}"; else fail "${what}"; fi
}

q() { printf %s "$1" | sed 's/ /+/g'; }
change() { gerrit "changes/?q=project:${P}+message:%22$(q "$1")%22" | jq -r '.[0]._number // empty'; }
change_field() { gerrit "changes/$1?o=CURRENT_REVISION&o=MESSAGES" | jq -r "$2"; }
status_of() { [[ "$(change_field "$1" .status)" == "$2" ]]; }
pr_for() { me api "repos/${R}/pulls?state=all&per_page=50" --jq ".[] | select(.title==\"$1\") | .number" | head -1; }
pr() { me api "repos/${R}/pulls/$1" --jq "$2"; }
gh_branch() { me api "repos/${R}/branches/$1" --jq .commit.sha 2>/dev/null; }
gerrit_branch() { gerrit "projects/${P}/branches/$1" | jq -r '.revision // empty'; }
followed() {  # Gerrit's branch is GitHub's
  local b=${1:-main} theirs; theirs=$(gh_branch "$b")
  [[ -n "${theirs}" && "$(gerrit_branch "$b")" == "${theirs}" ]]
}
# merged_at <change> <sha>: merged in Gerrit, with sha as its last patch set.
merged_at() { status_of "$1" MERGED && [[ "$(change_field "$1" .current_revision)" == "$2" ]]; }
has_change() { [[ -n "$(change "$1")" ]]; }
has_message() { change_field "$1" '.messages[].message' | grep -qF "$2"; }
check_run() {  # check_run <sha> <name> <conclusion>
  [[ "$(me api "repos/${R}/commits/$1/check-runs" --jq ".check_runs[] | select(.name==\"$2\") | .conclusion" | head -1)" == "$3" ]]
}
merge() {  # merge <pr> <method> [commit message]: the merge button, as AustinSchuh
  local args=(-X PUT "repos/${R}/pulls/$1/merge" -f merge_method="$2")
  [[ $# -gt 2 ]] && args+=(-f commit_message="$3")
  me api "${args[@]}" --jq .sha
}
# gh_pr <clone> <who> <branch> <base> <file> <subject> [content]: a commit
# on a new GitHub branch, and its PR.
gh_pr() {
  local dir=$1 who=$2 branch=$3 base=$4 file=$5 subject=$6 content=${7:-$6}
  (cd "${dir}" && git fetch -q github && git checkout -q -B "${branch}" "github/${base}" &&
    echo "${content}" > "${file}" && git add "${file}" && git commit -qs -m "${subject}" &&
    git push -q github "HEAD:refs/heads/${branch}") || return 1
  "${who}" pr create -R "${R}" --base "${base}" --head "${branch}" --title "${subject}" --body "Rehearsal ${RUN}: ${subject}" >/dev/null
}

# The clones, made on first use: GitHub as origin "github", Gerrit "gerrit".
clone() {  # clone <dir> <github login> <gerrit user> <email>
  [[ -d "$1/.git" ]] && return
  git clone -q -c "credential.https://github.com.helper=" \
    -c "credential.https://github.com.helper=!f() { test \"\$1\" = get && echo username=x-access-token && echo password=\$(${GH} auth token --user $2); }; f" \
    -o github "https://github.com/${R}.git" "$1"
  # Each URL's helpers start with an empty one: none of the global ones (a
  # keyring) are asked. An empty helper clears those before it, so it goes
  # first, and only for its own URL.
  (cd "$1" && git config user.name "Austin Schuh" && git config user.email "$4" &&
    git remote add gerrit "${G}/a/${P}" &&
    git config "credential.http://127.0.0.1:18080.username" "$3" &&
    git config --add "credential.http://127.0.0.1:18080.helper" "" &&
    git config --add "credential.http://127.0.0.1:18080.helper" "!f() { test \"\$1\" = get && echo password=\$(cat ${CREDS}/$3.http-password); }; f" &&
    curl -sf -o .git/hooks/commit-msg "${G}/tools/hooks/commit-msg" && chmod +x .git/hooks/commit-msg)
}
clone "${ME_CLONE}" AustinSchuh AustinSchuh "$(git config user.email)"
clone "${BRT_CLONE}" AustinSchuhBRT AustinSchuhBRT austin.schuh@bluerivertech.com

echo "Rehearsal (GitHub owns ${R}) ${RUN}"

step "0. Ready"
can_push() { [[ "$(me api "repos/${R}/collaborators/realtimeroboticsbot/permission" --jq .permission)" == write ]]; }
check "the bot may push to ${R}" can_push
wait_for "the GitHub-owned bridge is up" curl -sf -o /dev/null http://127.0.0.1:18083/healthz
# Only the GitHub-owned bridge's own account has Maintain Server; the
# Gerrit-owned one's, as AOS's, does not.
caps() { gerrit "accounts/$1/capabilities?q=maintainServer" | jq -r 'has("maintainServer")'; }
check "github-bridge-gh has Maintain Server" test "$(caps github-bridge-gh)" == true
check "github-bridge (the Gerrit-owned bridge) does not" test "$(caps github-bridge)" == false
# gh webhook forward exits while GitHub still has its last run's hook, and
# systemd restarts it: events in between are only swept up later.
forwarding() {
  local since now
  since=$(systemctl --user show bridge-test-gh-webhooks -p ActiveEnterTimestampMonotonic --value)
  now=$(awk '{printf "%d", $1 * 1000000}' /proc/uptime)
  systemctl --user -q is-active bridge-test-gh-webhooks && ((now - since > 20000000)) &&
    [[ "$(me api "repos/${R}/hooks" --jq '[.[] | select(.name == "cli" and .active)] | length')" == 1 ]]
}
wait_for "GitHub's webhooks are forwarded to it" forwarding
wait_for "Gerrit's main is GitHub's" followed main

# --- 1. A GitHub PR: imported, reviewed both ways, squashed by the button ---
step "1. AustinSchuhBRT's PR, squash-merged on GitHub"
S1="Squash ${RUN}"
gh_pr "${BRT_CLONE}" brt "gh-${RUN}-1" main "squash-${RUN}.txt" "${S1}"
N1=$(pr_for "${S1}")
imported() { C1=$(change "${S1}"); [[ -n "${C1}" ]] && [[ "$(change_field "${C1}" .owner._account_id)" == "$(gerrit 'accounts/AustinSchuhBRT' | jq ._account_id)" ]]; }
wait_for "PR #${N1} becomes a change owned by AustinSchuhBRT" imported
C1=$(change "${S1}")
trailer() { [[ "$(pr "$1" .body | tail -1)" == "Change-Id: $(change_field "$2" .change_id)" ]]; }
wait_for "the PR's description ends with change ${C1}'s Change-Id" trailer "${N1}" "${C1}"
H1=$(pr "${N1}" .head.sha)
wait_for "GitHub Actions builds it (check run build: success)" check_run "${H1}" build success
anonymous() {
  [[ "$(curl -s -o /dev/null -w '%{http_code}' "${FRONT}/bridge-${P}/checks/${C1}/1")" == 401 ]] &&
    [[ "$(curl -s -o /dev/null -w '%{http_code}' -b GerritAccount=aMadeUpSessionValue "${FRONT}/bridge-${P}/checks/${C1}/1")" == 401 ]]
}
check "the Checks tab, through Gerrit's path, answers nobody signed out or with a made-up session" anonymous
gerrit_as AustinSchuh POST "changes/${C1}/revisions/current/review" \
  "{\"comments\":{\"squash-${RUN}.txt\":[{\"line\":1,\"message\":\"Rehearsal ${RUN}: why this line?\",\"unresolved\":true}]}}" >/dev/null
ghc() { me api "repos/${R}/pulls/${N1}/comments" --jq ".[] | select(.body | contains(\"Rehearsal ${RUN}: why this line\")) | $1" | head -1; }
by_him() { [[ "$(ghc .user.login)" == AustinSchuh ]]; }
wait_for "a Gerrit comment appears on GitHub, as AustinSchuh (linked)" by_him
brt api "repos/${R}/pulls/${N1}/comments/$(ghc .id)/replies" -f body="Rehearsal ${RUN}: because." --silent
by_brt() { gerrit "changes/${C1}/comments" | jq -e ".[][] | select(.message == \"Rehearsal ${RUN}: because.\") | select(.author.username == \"AustinSchuhBRT\")"; }
wait_for "the GitHub reply lands in Gerrit's thread, as AustinSchuhBRT" by_brt
# /submit from a writer who has not linked (AustinSchuhBRT) is refused,
# and nothing merges: it would merge as them, and nothing says it may.
# A minute after the head reached GitHub, or it is refused for that.
read_it
brt api "repos/${R}/issues/${N1}/comments" -f body="/submit" --silent
refused_unlinked() { me api "repos/${R}/issues/${N1}/comments" --jq '.[].body' | grep -q "is not linked"; }
wait_for "/submit by an unlinked writer is refused" refused_unlinked
check "and the PR is not merged" test "$(pr "${N1}" .merged)" == false
M1=$(merge "${N1}" squash)
wait_for "Gerrit's main follows the squash" followed main
# GitHub adds the commits' sign-offs in a paragraph after it, so Gerrit
# does not close the change itself: the bridge's check-and-fix does.
has_id() { me api "repos/${R}/commits/$1" --jq .commit.message | grep -qx "Change-Id: $(change_field "$2" .change_id)"; }
check "GitHub's squash commit has the Change-Id (the description is the message)" has_id "${M1}" "${C1}"
wait_for "change ${C1} is merged, at GitHub's commit" merged_at "${C1}" "${M1}"
not_created() { ! gerrit "projects/${P}/branches/gh-${RUN}-1" | jq -e .revision; }
check "the PR's branch is not created in Gerrit" not_created

# --- 2. Two authors: GitHub adds Co-authored-by after the Change-Id ---
step "2. A PR with two authors, squashed"
S2="Coauthored ${RUN}"
gh_pr "${BRT_CLONE}" brt "gh-${RUN}-2" main "coauthored-${RUN}.txt" "${S2}"
N2=$(pr_for "${S2}")
wait_for "PR #${N2} becomes a change" has_change "${S2}"
C2=$(change "${S2}")
(cd "${ME_CLONE}" && git fetch -q github && git checkout -q -B "co-${RUN}" "github/gh-${RUN}-2" &&
  echo "more" >> "coauthored-${RUN}.txt" && git commit -qas -m "Rehearsal ${RUN}: AustinSchuh adds to it" &&
  git push -q github "HEAD:refs/heads/gh-${RUN}-2")
two_ps() { gerrit "changes/${C2}?o=ALL_REVISIONS" | jq -e '[.revisions[]._number] | max == 2'; }
wait_for "AustinSchuh's commit becomes patch set 2" two_ps
M2=$(merge "${N2}" squash)
coauthored() { me api "repos/${R}/commits/${M2}" --jq .commit.message | grep -q '^Co-authored-by:'; }
check "GitHub's squash commit ends with Co-authored-by (so Gerrit misses the Change-Id)" coauthored
wait_for "Gerrit's main follows" followed main
wait_for "the bridge marks change ${C2} merged at GitHub's commit (check-and-fix)" merged_at "${C2}" "${M2}"
wait_for "and says who merged it, in which PR" has_message "${C2}" "AustinSchuh in PR #${N2}, as ${M2} on main"

# --- 3. The Change-Id edited out in the merge dialog ---
step "3. A squash whose message lost the Change-Id"
S3="Edited ${RUN}"
gh_pr "${BRT_CLONE}" brt "gh-${RUN}-3" main "edited-${RUN}.txt" "${S3}"
N3=$(pr_for "${S3}")
wait_for "PR #${N3} becomes a change" has_change "${S3}"
C3=$(change "${S3}")
wait_for "its description has the Change-Id" trailer "${N3}" "${C3}"
M3=$(merge "${N3}" squash "Rehearsal ${RUN}: a message without a Change-Id")
wait_for "Gerrit's main follows" followed main
wait_for "change ${C3} is merged at GitHub's commit all the same" merged_at "${C3}" "${M3}"

# --- 4. A Gerrit change by a linked writer: a PR, updated, /submit ---
step "4. AustinSchuh's Gerrit change, merged with /submit"
S4="Gerrit-born ${RUN}"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "gb-${RUN}" gerrit/main &&
  echo "${S4}" > "gerrit-born-${RUN}.txt" && git add . && git commit -qs -m "${S4}" &&
  git push -q gerrit HEAD:refs/for/main 2>/dev/null)
C4=$(change "${S4}")
# Opened as its owner, with their linked account, never as the bot: GitHub
# then counts no approval of their own PR.
has_pr4() { N4=$(pr_for "${S4}"); [[ -n "${N4}" ]] && [[ "$(pr "${N4}" .user.login)" == AustinSchuh ]]; }
wait_for "change ${C4} becomes a PR opened as its owner" has_pr4
N4=$(pr_for "${S4}")
wait_for "its description ends with the Change-Id" trailer "${N4}" "${C4}"
OLD4=$(pr "${N4}" .head.sha)
(cd "${ME_CLONE}" && echo "again" >> "gerrit-born-${RUN}.txt" && git commit -qa --amend --no-edit &&
  git push -q gerrit HEAD:refs/for/main 2>/dev/null)
moved() { [[ "$(pr "${N4}" .head.sha)" != "${OLD4}" ]]; }
wait_for "patch set 2 reaches the PR" moved
H4=$(pr "${N4}" .head.sha)
wait_for "GitHub Actions builds it" check_run "${H4}" build success
read_it
me api "repos/${R}/issues/${N4}/comments" -f body="/submit" --silent
merged4() { [[ "$(pr "${N4}" .merged)" == true ]]; }
wait_for "/submit merges PR #${N4} on GitHub" merged4
check "as AustinSchuh, with his linked account" test "$(pr "${N4}" .merged_by.login)" == AustinSchuh
M4=$(pr "${N4}" .merge_commit_sha)
one_parent() { [[ "$(me api "repos/${R}/commits/${M4}" --jq '.parents | length')" == 1 ]]; }
check "squashed" one_parent
wait_for "Gerrit's main follows" followed main
wait_for "change ${C4} is merged at GitHub's commit" merged_at "${C4}" "${M4}"

# --- 5. A Gerrit change by someone with no linked account stays in Gerrit ---
step "5. bob's Gerrit change (not linked)"
S5="Unlinked ${RUN}"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "ul-${RUN}" gerrit/main &&
  echo "${S5}" > "unlinked-${RUN}.txt" && git add . &&
  git -c user.name=Bob -c user.email=bob@example.com commit -q -m "${S5}" -m "Signed-off-by: Bob <bob@example.com>" &&
  git -c "credential.http://127.0.0.1:18080.username=bob" -c "credential.http://127.0.0.1:18080.helper=" \
    -c "credential.http://127.0.0.1:18080.helper=!f() { test \"\$1\" = get && echo password=\$(cat ${CREDS}/bob.http-password); }; f" \
    push -q gerrit HEAD:refs/for/main 2>/dev/null)
B5=$(cd "${ME_CLONE}" && git rev-parse HEAD)
withheld() { C5=$(change "${S5}"); [[ -n "${C5}" ]] && has_message "${C5}" "GitHub owns this repository's branches"; }
wait_for "bob's change is withheld, and says why (link a GitHub account)" withheld
# What a withheld change must not do is push: a branch runs workflows.
# GitHub's own "no such commit" (422, or 404), not any failure.
# (Not a pipe: under pipefail gh's own failure would fail it.)
nowhere() { local out; out=$(me api "repos/${R}/commits/$1" --silent 2>&1); [[ "${out}" == *"(HTTP 404)"* || "${out}" == *"(HTTP 422)"* ]]; }
check "and has no PR" test -z "$(pr_for "${S5}")"
check "and its commit is on no GitHub branch" nowhere "${B5}"
# A linked writer's change on top of it is withheld too, and stays so
# when bob's patch set is outdated (the parent's code is still bob's).
S5b="On unlinked ${RUN}"
(cd "${ME_CLONE}" && echo "${S5b}" > "on-unlinked-${RUN}.txt" && git add . && git commit -qs -m "${S5b}" &&
  git push -q gerrit HEAD:refs/for/main 2>/dev/null) || fail "setup: pushing ${S5b}"
B5b=$(cd "${ME_CLONE}" && git rev-parse HEAD)
child_withheld() { C5b=$(change "${S5b}"); [[ -n "${C5b}" ]] && has_message "${C5b}" "Here, not: change ${C5}'s patch set 1 below it"; }
wait_for "AustinSchuh's change on it is withheld too" child_withheld
(cd "${ME_CLONE}" && git checkout -q HEAD^ && echo "${S5} v2" > "unlinked-${RUN}.txt" && git add . &&
  git -c user.name=Bob -c user.email=bob@example.com commit -q --amend --no-edit &&
  git -c "credential.http://127.0.0.1:18080.username=bob" -c "credential.http://127.0.0.1:18080.helper=" \
    -c "credential.http://127.0.0.1:18080.helper=!f() { test \"\$1\" = get && echo password=\$(cat ${CREDS}/bob.http-password); }; f" \
    push -q gerrit HEAD:refs/for/main 2>/dev/null) || fail "setup: bob's patch set 2"
bob_ps2() { [[ "$(change_field "${C5}" .current_revision)" != "${B5}" ]]; }
wait_for "bob uploads patch set 2" bob_ps2
# The bridge has looked since: its notice on bob's patch set 2.
bob_ps2_seen() { has_message "${C5}" "Patch set 2 is not mirrored"; }
wait_for "and the bridge has looked at it" bob_ps2_seen
check "AustinSchuh's change still has no PR" test -z "$(pr_for "${S5b}")"
check "and neither bob's old commit nor it is on GitHub" eval 'nowhere "${B5}" && nowhere "${B5b}"'

# --- 6. A subtree pulled in with git subtree --squash, merged with /submit ---
step "6. A subtree, merged with a merge commit by /submit"
S6="Subtree ${RUN}"
SRC=$(mktemp -d "${HOME}/tmp/subtree-src-XXXX")
(cd "${SRC}" && git init -q -b main && echo "library ${RUN}" > lib.txt && git add . &&
  git -c user.name=Library -c user.email=lib@example.com commit -q -m "Library ${RUN}")
(cd "${ME_CLONE}" && git fetch -q github && git checkout -q -B "gh-${RUN}-6" github/main &&
  git subtree add -q --squash --prefix="third_party/lib-${RUN}" "${SRC}" main -m "${S6}" >/dev/null 2>&1 &&
  git push -q github "HEAD:refs/heads/gh-${RUN}-6")
rm -rf "${SRC}"
me pr create -R "${R}" --base main --head "gh-${RUN}-6" --title "${S6}" --body "Rehearsal ${RUN}: a subtree" >/dev/null
N6=$(pr_for "${S6}")
wait_for "PR #${N6} becomes a change" has_change "${S6}"
C6=$(change "${S6}")
wait_for "its description has the Change-Id" trailer "${N6}" "${C6}"
read_it
REV6=$(brt api "repos/${R}/pulls/${N6}/reviews" -f event=APPROVE -f body="Rehearsal ${RUN}: LGTM" --jq .id)
brt_cr() { gerrit "changes/${C6}?o=DETAILED_LABELS&o=DETAILED_ACCOUNTS" | jq -e '[.labels["Code-Review"].all[]? | select(.username == "AustinSchuhBRT") | (.value // 0)] | .[0] == 1'; }
wait_for "AustinSchuhBRT's GitHub approval shows in Gerrit as their Code-Review +1" brt_cr
read_it
me api "repos/${R}/issues/${N6}/comments" -f body="/submit" --silent
merged6() { [[ "$(pr "${N6}" .merged)" == true ]]; }
wait_for "/submit merges PR #${N6}" merged6
M6=$(pr "${N6}" .merge_commit_sha)
two_parents() { [[ "$(me api "repos/${R}/commits/${M6}" --jq '.parents | length')" == 2 ]]; }
check "with a merge commit (the PR has a subtree)" two_parents
wait_for "Gerrit's main follows" followed main
wait_for "change ${C6} is merged at the merge commit" merged_at "${C6}" "${M6}"

# --- 7. A stack: the bridge stacks a PR based on another's branch ---
step "7. A stack: /submit refuses the top, merges the bottom, then the top"
S7a="Stack bottom ${RUN}"; S7b="Stack top ${RUN}"
gh_pr "${BRT_CLONE}" brt "gh-${RUN}-7a" main "stack-a-${RUN}.txt" "${S7a}"
(cd "${BRT_CLONE}" && git checkout -q -B "gh-${RUN}-7b" && echo top > "stack-b-${RUN}.txt" && git add . &&
  git commit -qs -m "${S7b}" && git push -q github "HEAD:refs/heads/gh-${RUN}-7b")
brt pr create -R "${R}" --base "gh-${RUN}-7a" --head "gh-${RUN}-7b" --title "${S7b}" --body "Rehearsal ${RUN}: on top" >/dev/null
N7a=$(pr_for "${S7a}"); N7b=$(pr_for "${S7b}")
both() { C7a=$(change "${S7a}"); C7b=$(change "${S7b}"); [[ -n "${C7a}" && -n "${C7b}" ]]; }
wait_for "PRs #${N7a} and #${N7b} become changes" both
C7a=$(change "${S7a}"); C7b=$(change "${S7b}")
stacked() { [[ "$(pr "${N7b}" .stack.size)" == 2 ]]; }
wait_for "the bridge stacks them" stacked
both_built() { check_run "$(pr "${N7a}" .head.sha)" build success && check_run "$(pr "${N7b}" .head.sha)" build success; }
wait_for "both build" both_built
read_it
for n in "${N7a}" "${N7b}"; do me api "repos/${R}/pulls/${n}/reviews" -f event=APPROVE -f body="Rehearsal ${RUN}: LGTM" --silent; done
me api "repos/${R}/issues/${N7b}/comments" -f body="/submit" --silent
said() { me api "repos/${R}/issues/$1/comments" --jq '.[].body' | grep -q "$2"; }
wait_for "/submit on the top is refused (merge the PRs below it first)" said "${N7b}" "Merge the PRs below it first"
me api "repos/${R}/issues/${N7a}/comments" -f body="/submit" --silent
merged7a() { [[ "$(pr "${N7a}" .merged)" == true ]]; }
wait_for "/submit merges the bottom, PR #${N7a} (GitHub's asynchronous merge)" merged7a
M7a=$(pr "${N7a}" .merge_commit_sha)
wait_for "change ${C7a} is merged" merged_at "${C7a}" "${M7a}"
check "the top, PR #${N7b}, is still open (only the bottom merged)" test "$(pr "${N7b}" .state)" == open
on_main() { [[ "$(pr "${N7b}" .base.ref)" == main ]]; }
wait_for "the top is now based on main" on_main
read_it
me api "repos/${R}/issues/${N7b}/comments" -f body="/submit" --silent
merged7b() { [[ "$(pr "${N7b}" .merged)" == true ]]; }
wait_for "/submit merges the top, PR #${N7b}" merged7b
M7b=$(pr "${N7b}" .merge_commit_sha)
wait_for "Gerrit's main follows" followed main
wait_for "change ${C7b} is merged" merged_at "${C7b}" "${M7b}"

# --- 8. CI failing, closing and reopening ---
step "8. A PR whose build fails; closed and reopened"
S8="Closed ${RUN}"
gh_pr "${BRT_CLONE}" brt "gh-${RUN}-8" main "closed-${RUN}.txt" "${S8}" "CI-MUST-FAIL ${RUN}"
N8=$(pr_for "${S8}")
wait_for "PR #${N8} becomes a change" has_change "${S8}"
C8=$(change "${S8}")
wait_for "its build fails on GitHub (check run build: failure)" check_run "$(pr "${N8}" .head.sha)" build failure
blocked() { [[ "$(GH_TOKEN="$("${GH}" auth token --user AustinSchuhBRT)" "${GH}" api "repos/${R}/pulls/${N8}" --jq .mergeable_state)" == blocked ]]; }
wait_for "GitHub blocks its merge (no approval, build failed)" blocked
brt pr close -R "${R}" "${N8}" >/dev/null
wait_for "closing it abandons change ${C8}" status_of "${C8}" ABANDONED
brt pr reopen -R "${R}" "${N8}" >/dev/null
wait_for "reopening it restores the change" status_of "${C8}" NEW
brt pr close -R "${R}" "${N8}" >/dev/null

# --- 9. A force-push of GitHub's main is followed ---
step "9. Force-push GitHub's main back, then forward"
TOP=$(gh_branch main); BACK=$(me api "repos/${R}/commits/${TOP}" --jq '.parents[0].sha')
me api -X PATCH "repos/${R}/git/refs/heads/main" -f sha="${BACK}" -F force=true --silent
at() { [[ "$(gerrit_branch main)" == "$1" ]]; }
wait_for "Gerrit's main follows the force-push" at "${BACK}"
me api -X PATCH "repos/${R}/git/refs/heads/main" -f sha="${TOP}" --silent
wait_for "and forward again" at "${TOP}"

# --- 10. Other branches follow only forward ---
step "10. A branch other than the default"
if [[ -z "$(gerrit_branch stable)" ]]; then
  gerrit_as AustinSchuh PUT "projects/${P}/branches/stable" "{\"revision\":\"$(gerrit_branch main)\"}" >/dev/null
fi
GS=$(gerrit_branch stable)
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "stable-${RUN}" gerrit/stable &&
  echo "${RUN}" > "stable-${RUN}.txt" && git add . && git commit -qs -m "Stable forward ${RUN}" && git push -q -f github HEAD:refs/heads/stable)
FWD=$(cd "${ME_CLONE}" && git rev-parse HEAD)
branch_at() { [[ "$(gerrit_branch "$1")" == "$2" ]]; }
wait_for "a forward push to GitHub's stable is followed" branch_at stable "${FWD}"
REFUSALS=$(grep 'only the default branch follows a force-push' "${LOG}" | grep -c 'branch=stable')
(cd "${ME_CLONE}" && git checkout -q -B "stable-side-${RUN}" "${GS}" && echo side > "stable-side-${RUN}.txt" && git add . &&
  git commit -qs -m "Stable sideways ${RUN}" && git push -q -f github HEAD:refs/heads/stable)
refused() { (($(grep 'only the default branch follows a force-push' "${LOG}" | grep -c 'branch=stable') > REFUSALS)); }
wait_for "a force-push of it is not followed (logged)" refused
check "Gerrit's stable stays where it was" test "$(gerrit_branch stable)" == "${FWD}"

step "11. Reviewers and whose turn it is, both ways"
S11="Reviewers ${RUN}"
(cd "${ME_CLONE}" && git fetch -q gerrit && git checkout -q -B "rv-${RUN}" gerrit/main &&
  echo "${S11}" > "reviewers-${RUN}.txt" && git add . && git commit -qs -m "${S11}" &&
  git push -q gerrit "HEAD:refs/for/main%r=AustinSchuhBRT" 2>/dev/null)
C11=$(change "${S11}")
has_pr11() { [[ -n "$(pr_for "${S11}")" ]]; }
wait_for "change ${C11} becomes a PR" has_pr11
N11=$(pr_for "${S11}")
asked() { pr "${N11}" '.requested_reviewers[].login' | grep -qx "$1"; }
not_asked() { ! asked "$1"; }
turn() { gerrit "changes/${C11}?o=DETAILED_ACCOUNTS" | jq -e ".attention_set // {} | .[] | select(.account.username == \"$1\")"; }
not_turn() { ! turn "$1"; }
reviewer() { gerrit "changes/${C11}/reviewers/" | jq -e ".[] | select(.username == \"$1\")"; }
wait_for "%r=AustinSchuhBRT asks AustinSchuhBRT to review PR #${N11}" asked AustinSchuhBRT
# Their review ends the request, and their turn in Gerrit; nobody asks again.
brt api "repos/${R}/pulls/${N11}/reviews" -f event=COMMENT -f body="Rehearsal ${RUN}: looked." --silent
# A review's body is a patch-set-level comment in Gerrit.
looked() { gerrit "changes/${C11}/comments" | jq -e "[.[][] | select(.message | contains(\"Rehearsal ${RUN}: looked.\"))] | length > 0"; }
wait_for "AustinSchuhBRT's review reaches Gerrit" looked
wait_for "and it is no longer their turn in Gerrit" not_turn AustinSchuhBRT
read_it
check "nor are they asked again on GitHub" not_asked AustinSchuhBRT
# AustinSchuh replies in Gerrit, handing them the turn: asked again.
gerrit_as AustinSchuh POST "changes/${C11}/revisions/current/review" \
  '{"message":"PTAL","add_to_attention_set":[{"user":"AustinSchuhBRT","reason":"PTAL"}]}' >/dev/null
wait_for "AustinSchuh handing them the turn in Gerrit asks them again" asked AustinSchuhBRT
# Removed as a reviewer in Gerrit: the request is withdrawn.
BRT_ID=$(gerrit 'accounts/AustinSchuhBRT' | jq ._account_id)
# A body: with gerrit_as's JSON content type, Gerrit refuses an empty one.
gerrit_as AustinSchuh DELETE "changes/${C11}/reviewers/${BRT_ID}" '{}' >/dev/null
wait_for "removing them as a Gerrit reviewer withdraws it" not_asked AustinSchuhBRT
check "and they are no longer a Gerrit reviewer" eval '! reviewer AustinSchuhBRT'
# Asked on GitHub: a Gerrit reviewer, whose turn it is; withdrawn: not.
me api -X POST "repos/${R}/pulls/${N11}/requested_reviewers" -f 'reviewers[]=AustinSchuhBRT' --silent
wait_for "asking them on GitHub makes them a Gerrit reviewer" reviewer AustinSchuhBRT
wait_for "whose turn it is" turn AustinSchuhBRT
me api -X DELETE "repos/${R}/pulls/${N11}/requested_reviewers" -f 'reviewers[]=AustinSchuhBRT' --silent
wait_for "withdrawing it on GitHub ends their turn in Gerrit" not_turn AustinSchuhBRT
check "and leaves them a reviewer" reviewer AustinSchuhBRT
# Changes requested on GitHub; a new patch set in Gerrit asks them again.
brt api "repos/${R}/pulls/${N11}/reviews" -f event=REQUEST_CHANGES -f body="Rehearsal ${RUN}: fix it." --silent
fixit() { gerrit "changes/${C11}/comments" | jq -e "[.[][] | select(.message | contains(\"Rehearsal ${RUN}: fix it.\"))] | length > 0"; }
wait_for "AustinSchuhBRT's change request reaches Gerrit" fixit
read_it
check "and they are not asked meanwhile" not_asked AustinSchuhBRT
(cd "${ME_CLONE}" && echo "fixed" >> "reviewers-${RUN}.txt" && git commit -qa --amend --no-edit &&
  git push -q gerrit HEAD:refs/for/main 2>/dev/null)
wait_for "AustinSchuh's new patch set asks them again" asked AustinSchuhBRT
# A PR worked on GitHub alone (step 1's) never had the bridge ask or
# withdraw anyone's review.
bot_asked() { me api "repos/${R}/issues/$1/timeline" --paginate --jq '.[] | select(.event == "review_requested" or .event == "review_request_removed") | .actor.login' | grep -qx realtimeroboticsbot; }
check "the bridge asked nobody to review PR #${N1}, worked on GitHub alone" eval "! bot_asked ${N1}"

echo
echo "Rehearsal (GitHub owns ${R}) ${RUN}: ${PASSED} passed, ${FAILED} failed"
((FAILED == 0))
