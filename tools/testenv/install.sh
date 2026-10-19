#!/bin/bash
# Installs the bridge's test instance as systemd user services:
#
#   bridge-test-gerrit     test Gerrit (127.0.0.1:18080, ssh :29418), replicating
#                          bridge-scratch's branches to GitHub
#   bridge-test-buildkite  fake Buildkite (127.0.0.1:18090)
#   bridge-test-ci         the real gerrit-buildkite, pointed at both
#   bridge-test-bridge     the bridge itself
#   bridge-test-webhooks   GitHub webhooks forwarded to the bridge (only if
#                          the gh token has admin:repo_hook)
#
# Logs go to ~/tmp/bridge-test/logs/. Re-run after changing the code to
# rebuild and restart everything.
set -euo pipefail

WS="$(cd "$(dirname "$0")/../.." && pwd)"
TE="${HOME}/tmp/bridge-test"
SITE="${HOME}/tmp/gerrit-test"
GBK="${GERRIT_BUILDKITE:-${HOME}/tmp/gerrit-buildkite/gerrit-buildkite}"
GH="${GH:-${HOME}/bin/gh}"
REPO="${REPO:-AustinSchuh/bridge-scratch}"
# GitHub accounts: the bridge's bot, and a second human (bob in Gerrit).
BOT="${BOT:-realtimeroboticsbot}"
SECOND="${SECOND:-AustinSchuhBRT}"
UNITS="${HOME}/.config/systemd/user"

mkdir -p "${TE}/bin" "${TE}/logs" "${UNITS}"
cd "${WS}"
for t in testgerrit fakebuildkite bridge; do
  bazel run --script_path="${TE}/bin/${t}" "//cmd/${t}" 2>/dev/null
done
umask 077
# Replication pushes as the bot, like a production replication account.
"${GH}" auth token --user "${BOT}" > "${TE}/github-token"
# Replication pushes main with a deploy key: the repository's ruleset lets
# only deploy keys update main (as production lets only the bridge), so
# nobody can merge on GitHub behind Gerrit's back.
KEY="${TE}/replication-deploy-key"
[[ -f "${KEY}" ]] || ssh-keygen -q -t ed25519 -N '' -C "bridge-test replication" -f "${KEY}"
if ! "${GH}" api "repos/${REPO}/keys" --jq '.[].key' | grep -qF "$(cut -d' ' -f1-2 "${KEY}.pub")"; then
  "${GH}" api "repos/${REPO}/keys" -f title="Gerrit replication (bridge test)" -f key="$(cat "${KEY}.pub")" -F read_only=false --silent
fi
"${GH}" api meta --jq '.ssh_keys[] | "github.com " + .' > "${TE}/github-known-hosts"
RULESET='{"name":"main belongs to Gerrit","target":"branch","enforcement":"active",
  "conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"],"exclude":[]}},
  "bypass_actors":[{"actor_type":"DeployKey","bypass_mode":"always"}],
  "rules":[{"type":"update"},{"type":"deletion"},{"type":"non_fast_forward"},
    {"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,
      "required_status_checks":[{"context":"gerrit/submittable"}]}}]}'
RID="$("${GH}" api "repos/${REPO}/rulesets" --jq '.[] | select(.name == "main belongs to Gerrit") | .id')"
if [[ -n "${RID}" ]]; then
  "${GH}" api -X PUT "repos/${REPO}/rulesets/${RID}" --input - --silent <<<"${RULESET}"
else
  "${GH}" api "repos/${REPO}/rulesets" --input - --silent <<<"${RULESET}"
fi
SECOND_EMAIL="$("${GH}" api "users/${SECOND}" --jq '"\(.id)+\(.login)@users.noreply.github.com"')"
# The second account's own address, if any (becomes its preferred email).
[[ -n "${SECOND_EMAIL_PREFERRED:-}" ]] && SECOND_EMAIL="${SECOND_EMAIL}|${SECOND_EMAIL_PREFERRED}"

unit() {  # name description exec [extra [Unit] lines] [extra [Service] lines]
  cat > "${UNITS}/$1.service" <<UNIT
[Unit]
Description=$2
$4

[Service]
ExecStart=$3
Restart=on-failure
RestartSec=5
StandardOutput=append:${TE}/logs/$1.log
StandardError=inherit
$5

[Install]
WantedBy=default.target
UNIT
}

unit bridge-test-gerrit "Bridge test instance: Gerrit" \
  "${TE}/bin/testgerrit --replicate_project bridge-scratch --github_token_file ${TE}/github-token --github_deploy_key_file ${KEY} --github_known_hosts_file ${TE}/github-known-hosts --ci_user buildkite --buildkite_url http://127.0.0.1:18090 --github ${REPO} --github_bot ${BOT} --identity AustinSchuh=AustinSchuh --base_path /gerrit --github_login_jars ${HOME}/tmp/github-plugin --github_oauth_file ${TE}/github-oauth --github_webhook_user AustinSchuh --github_login_users AustinSchuh|trusted|admin|$(git -C "${WS}" config user.email),${SECOND}|${SECOND_EMAIL}" \
  "" "TimeoutStopSec=60"
unit bridge-test-buildkite "Bridge test instance: fake Buildkite" \
  "${TE}/bin/fakebuildkite --duration 20s --gerrit_url http://127.0.0.1:18080/gerrit" "" ""
unit bridge-test-ci "Bridge test instance: gerrit-buildkite" \
  "${GBK} -token fake -api_url http://127.0.0.1:18090/ -webhook_token test-webhook-token -user buildkite -key ${SITE}/ci/id_ed25519 -gerrit_url http://127.0.0.1:18080/gerrit -http_password_file ${SITE}/ci/http-password -server localhost -project bridge-scratch -buildkite_project ci -organization test -database ${TE}/buildkite.db" \
  "After=bridge-test-gerrit.service bridge-test-buildkite.service" \
  "Environment=PATH=${SITE}/ci/bin:/usr/bin:/bin"
unit bridge-test-bridge "Bridge test instance: the bridge" \
  "${TE}/bin/bridge --config ${SITE}/bridge/bridge-scratch.yaml --dry_run=false${BRIDGE_VERBOSE:+ -v}" \
  "After=bridge-test-gerrit.service" \
  "Environment=PATH=$(dirname "${GH}"):/usr/bin:/bin"
if "${GH}" auth status 2>&1 | grep -q 'admin:repo_hook'; then
  unit bridge-test-webhooks "Bridge test instance: GitHub webhook forwarding" \
    "${WS}/tools/testenv/webhook-forward.sh ${GH} ${REPO} pull_request,pull_request_review,pull_request_review_comment,pull_request_review_thread,issue_comment,push http://127.0.0.1:18081/webhook ${SITE}/bridge/webhook-secret" \
    "After=bridge-test-bridge.service" "Environment=PATH=$(dirname "${GH}"):/usr/bin:/bin"
fi

systemctl --user daemon-reload
for s in gerrit buildkite ci bridge webhooks; do
  [[ -f "${UNITS}/bridge-test-${s}.service" ]] || continue
  systemctl --user enable --quiet "bridge-test-${s}.service"
  systemctl --user restart "bridge-test-${s}.service"
  [[ "${s}" == gerrit ]] && for _ in $(seq 180); do curl -sf -o /dev/null http://127.0.0.1:18080/gerrit/config/server/version && break; sleep 1; done
done
systemctl --user --no-pager --type=service list-units 'bridge-test-*'
