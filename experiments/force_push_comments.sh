#!/bin/bash
# Experiment: what happens to GitHub review comments when a stacked PR's
# branch is force-pushed (as the bridge does when Gerrit uploads a new patch
# set), and can the bridge still post a comment against a commit that has
# been force-pushed away (a Gerrit comment on an old patch set)?
#
# Usage:
#   GITHUB_TOKEN=... REPO=owner/scratch-repo ./force_push_comments.sh
#
# REPO must be an existing, EMPTY scratch repository; the script pushes
# main, layer1 and layer2 to it.  The token needs contents + pull_requests
# write (classic: "repo").

set -euo pipefail

: "${GITHUB_TOKEN:?set GITHUB_TOKEN}"
: "${REPO:?set REPO=owner/name}"
API=https://api.github.com
SETTLE="${SETTLE:-15}"  # seconds to let GitHub recompute comment positions

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
cd "${WORK}"

gh_api() {
  local method="$1" path="$2"
  shift 2
  curl -sS -X "${method}" \
    -H "Authorization: Bearer ${GITHUB_TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "${API}${path}" "$@"
}

json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

log() { printf '\n=== %s\n' "$*"; }

# Writes a.txt with 60 numbered lines, applying sed edits passed as args.
write_file() {
  seq -f 'line %g' 1 60 > a.txt
  for e in "$@"; do sed -i "${e}" a.txt; done
}

git init -q -b main repo && cd repo
git config credential.helper ""
git config user.name "bridge experiment"
git config user.email "bridge@example.invalid"
git remote add origin "https://x-access-token:${GITHUB_TOKEN}@github.com/${REPO}.git"

log "Creating main, layer1 (edits lines 10-12), layer2 (edits lines 30-32)"
write_file
git add a.txt && git commit -qm "base"
git push -q -f origin main

git checkout -qb layer1
write_file '10,12s/$/ L1/'
git commit -qam "layer1"
L1_V1="$(git rev-parse HEAD)"

git checkout -qb layer2
write_file '10,12s/$/ L1/' '30,32s/$/ L2/'
git commit -qam "layer2"
L2_V1="$(git rev-parse HEAD)"
git push -q -f origin layer1 layer2

create_pr() {
  gh_api POST "/repos/${REPO}/pulls" \
    -d "{\"title\":\"$1\",\"head\":\"$1\",\"base\":\"$2\",\"body\":\"bridge experiment\"}" |
    json 'd.get("number", d)'
}
PR1="$(create_pr layer1 main)"
PR2="$(create_pr layer2 layer1)"
echo "PR1=#${PR1} PR2=#${PR2}"

log "Creating native stack [PR1, PR2]"
gh_api POST "/repos/${REPO}/stacks" -d "{\"pull_requests\":[${PR1},${PR2}]}" |
  json '{k: d.get(k) for k in ("number","message","pull_requests")}' || true

comment() {  # pr commit line body -> prints status + id
  gh_api POST "/repos/${REPO}/pulls/$1/comments" \
    -d "{\"commit_id\":\"$2\",\"path\":\"a.txt\",\"line\":$3,\"side\":\"RIGHT\",\"body\":\"$4\"}" |
    json '("id=%s" % d["id"]) if "id" in d else ("ERROR %s %s" % (d.get("message"), d.get("errors")))'
}

log "Commenting on PR2 line 31 (will be untouched), 32 (will change), and PR1 line 11"
comment "${PR2}" "${L2_V1}" 31 "C-untouched: PR2 line 31"
comment "${PR2}" "${L2_V1}" 32 "C-changed: PR2 line 32"
comment "${PR1}" "${L1_V1}" 11 "C-pr1: PR1 line 11"

dump() {
  sleep "${SETTLE}"
  log "State: $1"
  local pr
  for pr in "${PR1}" "${PR2}"; do
    gh_api POST /graphql -d "$(python3 -c '
import json,sys
o,n=sys.argv[1].split("/")
q="""query($o:String!,$n:String!,$pr:Int!){repository(owner:$o,name:$n){pullRequest(number:$pr){
  number headRefOid baseRefName
  reviewThreads(first:50){nodes{isOutdated isResolved line originalLine diffSide
    comments(first:10){nodes{databaseId body outdated commit{oid} originalCommit{oid}}}}}}}}"""
print(json.dumps({"query":q,"variables":{"o":o,"n":n,"pr":int(sys.argv[2])}}))' "${REPO}" "${pr}")" |
      python3 -c '
import json,sys
d=json.load(sys.stdin)
if "errors" in d: print(d["errors"]); sys.exit()
pr=d["data"]["repository"]["pullRequest"]
print("PR #%d head=%s base=%s" % (pr["number"], pr["headRefOid"][:8], pr["baseRefName"]))
for t in pr["reviewThreads"]["nodes"]:
  for c in t["comments"]["nodes"]:
    print("  %-28s thread.isOutdated=%-5s line=%-4s origLine=%-4s commit=%s origCommit=%s" % (
      c["body"][:28], t["isOutdated"], t["line"], t["originalLine"],
      (c["commit"] or {}).get("oid","-")[:8], (c["originalCommit"] or {}).get("oid","-")[:8]))'
  done
}

dump "initial"

log "Case A: force-push PR2 with a new patch set that only changes line 45"
git checkout -q layer2
write_file '10,12s/$/ L1/' '30,32s/$/ L2/' '45s/$/ L2-ps2/'
git commit -q --amend -a --no-edit
L2_V2="$(git rev-parse HEAD)"
git push -q -f origin layer2
dump "after A (line 31/32 hunks unchanged)"

log "Case B: force-push PR2 with a patch set that changes line 32"
write_file '10,12s/$/ L1/' '30,32s/$/ L2/' '45s/$/ L2-ps2/' '32s/$/ edited/'
git commit -q --amend -a --no-edit
L2_V3="$(git rev-parse HEAD)"
git push -q -f origin layer2
dump "after B (line 32 changed)"

log "Case C: new patch set of PR1 (line 50), PR2 rebased onto it; PR2's own diff unchanged"
git checkout -q layer1
write_file '10,12s/$/ L1/' '50s/$/ L1-ps2/'
git commit -q --amend -a --no-edit
git checkout -q layer2
write_file '10,12s/$/ L1/' '50s/$/ L1-ps2/' '30,32s/$/ L2/' '45s/$/ L2-ps2/' '32s/$/ edited/'
git reset -q --soft layer1 && git commit -qam "layer2"
L2_V4="$(git rev-parse HEAD)"
git push -q -f origin layer1 layer2
dump "after C (pure rebase of PR2)"

log "Case D: post comments against commits that were force-pushed away"
echo -n "D1 PR2 old commit L2_V1, line 31: "; comment "${PR2}" "${L2_V1}" 31 "D1: on force-pushed-away commit"
echo -n "D2 PR2 old commit L2_V2, line 45: "; comment "${PR2}" "${L2_V2}" 45 "D2: on force-pushed-away commit"
echo -n "D3 PR2 never-in-PR commit (PR1 v1), line 11: "; comment "${PR2}" "${L1_V1}" 11 "D3: control"
echo -n "D4 PR2 current head L2_V4, line 31: "; comment "${PR2}" "${L2_V4}" 31 "D4: control on head"
echo -n "D5 review API, old commit L2_V1: "
gh_api POST "/repos/${REPO}/pulls/${PR2}/reviews" \
  -d "{\"commit_id\":\"${L2_V1}\",\"event\":\"COMMENT\",\"body\":\"D5 review\",\"comments\":[{\"path\":\"a.txt\",\"line\":31,\"side\":\"RIGHT\",\"body\":\"D5: review comment on old commit\"}]}" |
  json '("id=%s" % d["id"]) if "id" in d else ("ERROR %s %s" % (d.get("message"), d.get("errors")))'
dump "after D"

log "Done. PRs: https://github.com/${REPO}/pull/${PR1} https://github.com/${REPO}/pull/${PR2}"
echo "Commits: L1_V1=${L1_V1} L2_V1=${L2_V1} L2_V2=${L2_V2} L2_V3=${L2_V3} L2_V4=${L2_V4}"
