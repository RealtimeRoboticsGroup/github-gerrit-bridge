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
SECOND_EMAIL="$("${GH}" api "users/${SECOND}" --jq '"\(.id)+\(.login)@users.noreply.github.com"')"

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
  "${TE}/bin/testgerrit --replicate_project bridge-scratch --github_token_file ${TE}/github-token --ci_user buildkite --github ${REPO} --github_bot ${BOT} --base_path /gerrit --github_login_jars ${HOME}/tmp/github-plugin --github_oauth_file ${TE}/github-oauth --github_webhook_user AustinSchuh --github_login_users AustinSchuh|trusted|admin,${SECOND}" \
  "" "TimeoutStopSec=60"
unit bridge-test-buildkite "Bridge test instance: fake Buildkite" \
  "${TE}/bin/fakebuildkite --duration 20s --gerrit_url http://127.0.0.1:18080/gerrit" "" ""
unit bridge-test-ci "Bridge test instance: gerrit-buildkite" \
  "${GBK} -token fake -api_url http://127.0.0.1:18090/ -webhook_token test-webhook-token -user buildkite -key ${SITE}/ci/id_ed25519 -server localhost -project bridge-scratch -buildkite_project ci -organization test -database ${TE}/buildkite.db" \
  "After=bridge-test-gerrit.service bridge-test-buildkite.service" \
  "Environment=PATH=${SITE}/ci/bin:/usr/bin:/bin"
unit bridge-test-bridge "Bridge test instance: the bridge" \
  "${TE}/bin/bridge --config ${SITE}/bridge/bridge-scratch.yaml --dry_run=false" \
  "After=bridge-test-gerrit.service" \
  "Environment=PATH=$(dirname "${GH}"):/usr/bin:/bin"
if "${GH}" auth status 2>&1 | grep -q 'admin:repo_hook'; then
  unit bridge-test-webhooks "Bridge test instance: GitHub webhook forwarding" \
    "${GH} webhook forward --repo=${REPO} --events=pull_request,pull_request_review,pull_request_review_comment,pull_request_review_thread,issue_comment,push --url=http://127.0.0.1:18081/webhook --secret=$(cat "${SITE}/bridge/webhook-secret")" \
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
