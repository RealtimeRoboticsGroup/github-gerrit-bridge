# The bridge test instance

A complete, disposable copy of the production setup, for trying the bridge
by hand before it goes near the real Gerrit:

| Piece | Where | Real or stand-in |
|---|---|---|
| Gerrit 3.14.4 | `http://127.0.0.1:18080/gerrit/`, ssh `127.0.0.1:29418`, site `~/tmp/gerrit-test` | real, with AOS's github plugin build and settings: you log in with GitHub |
| Replication | Gerrit's bundled plugin → `github.com/AustinSchuh/bridge-scratch`, configured like AOS's (default 15s `replicationDelay`, `rescheduleDelay = 15`, heads and tags, `force`) | real |
| Other plugins | the rest of AOS's `plugins/` (all bundled in the war): codemirror-editor, commit-message-length-validator, delete-project, download-commands, gitiles, hooks, plugin-manager, reviewnotes, singleusergroup, webhooks | real |
| CI | your `gerrit-buildkite` (`~/tmp/gerrit-buildkite`, plus a local `-api_url` patch) | real |
| Buildkite | `cmd/fakebuildkite` on `http://127.0.0.1:18090` | stand-in |
| The bridge | `cmd/bridge`, config `~/tmp/gerrit-test/bridge/bridge-scratch.yaml` | real |
| GitHub webhooks | `gh webhook forward` → `127.0.0.1:18081/webhook` | real (needs `admin:repo_hook`) |
| GitHub | `AustinSchuh/bridge-scratch` | real |

Everything listens on 127.0.0.1 only. From your desktop, tunnel the web
UIs:

```
ssh -L 18080:127.0.0.1:18080 -L 18090:127.0.0.1:18090 server1868-4
```

then open <http://127.0.0.1:18080/gerrit/login> and sign in with GitHub
(the "Bridge test Gerrit" OAuth App, owned by realtimeroboticsbot), and
<http://127.0.0.1:18090> for builds. Use `127.0.0.1`, not `localhost`: the
OAuth callback goes to 127.0.0.1, and a session cookie set on another host
does not follow it ("Invalid authentication state"). Links the bridge posts
on GitHub point at these URLs too.

## Services

```
tools/testenv/install.sh                   # build, (re)install and restart everything
systemctl --user status 'bridge-test-*'
systemctl --user restart bridge-test-bridge  # after changing only the bridge (install.sh rebuilds)
journalctl is not used: logs are in ~/tmp/bridge-test/logs/<service>.log
```

| Service | What |
|---|---|
| `bridge-test-gerrit` | Gerrit; replication pushes `bridge-scratch`'s `refs/heads/*` to GitHub (force: Gerrit owns them) |
| `bridge-test-buildkite` | fake Buildkite |
| `bridge-test-ci` | gerrit-buildkite, using the `buildkite` account (`~/tmp/gerrit-test/ci/`) |
| `bridge-test-bridge` | the bridge, `--dry_run=false`; a full resync every 5 min backs up the webhooks |
| `bridge-test-webhooks` | `gh webhook forward`: GitHub events reach the bridge in seconds |

Gerrit's own logs are in `~/tmp/gerrit-test/logs/` (`replication_log`,
`sshd_log`, `error_log`).

## GitHub accounts

| GitHub account | Role | Gerrit account |
|---|---|---|
| `realtimeroboticsbot` | the bridge (PRs, mirrored comments, statuses) and replication | `github-bridge` |
| `AustinSchuh` | trusted developer; also forwards webhooks (repo admin) | `AustinSchuh` (Verified Users) |
| `AustinSchuhBRT` | newer contributor | `AustinSchuhBRT` |

As on AOS (Gerrit's github plugin), Gerrit accounts are named after GitHub
logins, and the bridge matches them by username (`identity.match_usernames`);
there is no hand-written map. A GitHub user's comments, replies, edits,
resolutions, approvals (Code-Review +2, or +1 if that is all they may give;
request changes = -1), uploads and `/submit` all happen in Gerrit **as their
account** through Run-As, so `retest` from GitHub starts CI for trusted
users. GitHub users without a Gerrit account are handled by the bridge
account, and never vote.

All three are logged in to `gh` here (`gh auth token --user <login>`);
keep `AustinSchuh` the active account, since the webhook forwarder uses
it. Two clones are ready, each pushing as one person on both sides:

| Clone | GitHub as | Gerrit as |
|---|---|---|
| `~/local/bridge-play` | AustinSchuh | AustinSchuh |
| `~/local/bridge-play-brt` | AustinSchuhBRT | AustinSchuhBRT |

To act as AustinSchuhBRT with `gh`, prefix commands with
`GH_TOKEN=$(gh auth token --user AustinSchuhBRT)`.

The bot creates the Gerrit-born PRs, so GitHub will not let it approve
them: Gerrit votes stay statuses there.

## Gerrit accounts

| Account | Who | Notes |
|---|---|---|
| `AustinSchuh`, `AustinSchuhBRT` | the people | created by the github plugin at their first GitHub login (username = GitHub login, an `external:github_oauth` token stored). testgerrit then adds AustinSchuh to Verified Users and Administrators, sets each one's email as a user would under Settings, and writes HTTP passwords to `~/tmp/gerrit-test/bridge/<login>.http-password` for the clones |
| `admin` | bootstrap administrator | HTTP password only (no GitHub login) |
| `github-bridge` | the bridge | Service Users, Run As, owns `refs/bridge/*` |
| `buildkite` | CI | Service Users (votes Verified) |
| `alice`, `bob` | Gerrit-only test users | from the first boot; not used by the GitHub flows |

The installer adds AustinSchuhBRT's own address if you give it:
`SECOND_EMAIL_PREFERRED=<their-address> tools/testenv/install.sh`.

## Rehearsal

`tools/testenv/rehearsal.sh` drives the whole thing as the real people on
both sides and checks every step: a Gerrit chain becoming a stacked set of
PRs built by CI; comments, replies, suggestions and resolution both ways
(as their authors, via Run-As); a partial push freezing a base and a
rebase rejoining; GitHub approvals becoming Code-Review +2 and `/submit`
merging the chain, with replication marking the PRs merged; a GitHub stack
by the untrusted contributor imported as theirs without CI; `retest` from
GitHub by a trusted reviewer; a push by someone else uploaded as the
pusher; draft toggles; and a chain submitted in Gerrit itself, where the
bridge must retarget the PRs to `main` within replication's 15 seconds for
GitHub to show them merged; and a PR from AustinSchuhBRT's fork
(`AustinSchuhBRT/bridge-scratch`, branch `main`), with Gerrit patch sets
pushed back to the fork while maintainer edits are allowed and reported
in `gerrit/sync` when they are not. Run it after any bridge change.

## Project rules (like AOS)

`bridge-scratch` uses AOS's `project.config` rules (copied 2026-10-10):

- **Signed-off-by** submit requirement: the commit message must contain
  `Signed-off-by`. Use `git commit -s`. For PRs opened on GitHub the bridge
  keeps the sign-offs of the layer's commits (and any in the PR
  description) when it squashes; `gerrit/sync` warns when there are none.
- **Verified** submit requirement: a +1 from someone other than the
  uploader, and no -1. Only Service Users (the CI account) may vote it,
  and any new patch set resets it (`copyCondition = changekind:NO_CHANGE`).
- Code-Review +2 (inherited from All-Projects).

AOS's label function `MaxWithBlock` can no longer be stored by Gerrit
3.14, so the test uses `NoBlock`; the explicit Verified submit requirement
makes them behave the same.

Your email is added to alice, so commits you make can be pushed as alice.

## Things to try

Work in a clone that can talk to both sides:

```
git clone http://alice:$(cat ~/tmp/gerrit-test/bridge/alice.http-password)@127.0.0.1:18080/a/bridge-scratch
cd bridge-scratch
curl -so .git/hooks/commit-msg http://127.0.0.1:18080/tools/hooks/commit-msg && chmod +x .git/hooks/commit-msg
git remote add github https://github.com/AustinSchuh/bridge-scratch.git
```

**From Gerrit**

- Upload a chain: commit two or three times, `git push origin HEAD:refs/for/main`.
  Each change gets a `gerrit/<Change-Id>` branch and a PR, stacked.
  CI runs (alice is trusted) and `gerrit/Verified` follows it on GitHub.
- Make a build fail: add a file whose path contains `FAIL`.
- Comment, reply, resolve, vote, attach an "Apply fix" suggestion: all
  show up on the PR.
- Amend only the middle change of a chain and push: the PR above moves to
  a `gerrit-base/…` branch and leaves the stack until you rebase.
- Abandon, restore, mark WIP / ready.
- Submit in Gerrit: replication lands it on GitHub's `main`, and the PRs
  close as merged.

**From GitHub**

- Push a branch (`git push github HEAD:refs/heads/my-feature`), open a PR
  against `main`, and stack more on top (`gh stack` or the UI). Each layer
  becomes one Gerrit change owned by alice; `gerrit/sync` on the PR says
  how the import went.
- Push more commits, restack, force-push: new patch sets.
- Review on GitHub: comments, suggestions (```` ```suggestion ````),
  replies, resolving, edits.
- Toggle draft / ready.
- Comment `/submit` to submit the change and everything below it, as alice.
- Comment `retest` on the PR (as AustinSchuh) to rerun CI: it is posted in
  Gerrit as AustinSchuh, whom CI trusts. From AustinSchuhBRT it is not.
- Approve or request changes on GitHub: it becomes AustinSchuh's
  Code-Review vote in Gerrit.

## Resetting

```
systemctl --user stop 'bridge-test-*'
rm -rf ~/tmp/gerrit-test ~/tmp/gerrit-test.home ~/tmp/bridge-test/buildkite.db
tools/testenv/install.sh
```

A fresh site recreates the accounts, creates `bridge-scratch` from GitHub's
current `main`, writes `~/tmp/gerrit-test/bridge/bridge-scratch.yaml` (with
`import_created_after` set to now, so existing GitHub PRs are left alone),
and sets up CI. Bridge state lives in the Gerrit site (`refs/bridge/*`), so
it goes with it; PRs the old site mirrored are then simply ignored.

The GitHub token used by replication is a copy of `gh auth token` in
`~/tmp/bridge-test/github-token` and the site's `etc/secure.config`; both
are mode 600, but delete them when you are done with the test instance.
