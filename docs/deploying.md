# Deploying the bridge

Two checklists: standing up a deployment where GitHub owns the branches,
with people's GitHub accounts in LDAP (team1868/software at
gerrit.spacecookies.dev), and upgrading a Gerrit-owned one (AOS) from
`main` (a01d401) to this branch. Why each step is there is in
docs/design.md, which this cites by section.

## Where GitHub owns the branches, with LDAP (team1868)

### Gerrit

1. **The bridge's account**, outside LDAP (a local or service account; it
   is never matched to anyone), in a group of its own, with a preferred
   email.
2. **§9.1's grants still apply**, except as below: global Run As and Stream
   Events through that group; on the project, Read on its branches, Push
   and Create on `refs/for/refs/heads/*`, Add Patch Set, Forge Author on
   `refs/heads/*`, and Read, Push, Create on `refs/bridge/*`, all
   exclusive. Not in Administrators, Service Users or the CI trusted
   group.
3. **§3.1's grants on top:** on the project's `refs/heads/*`, Push with
   force, Forge Committer, Push Merge Commits and Abandon for that group;
   and the global **Maintain Server** for it (the bridge refuses to start
   without it here; a Gerrit-owned bridge never needs it). Nobody gets
   Submit, and nobody but the bridge's group gets Push on `refs/heads/*`.
4. **Not from §9.1, which is for Gerrit-owned branches:** GitHub Actions
   stay on (GitHub's CI is the point), and GitHub's default branch is the
   team's to protect, not replication's to own. No replication of the
   project to GitHub.
5. **`ci.trusted_group`** (default `Verified Users`) must exist, be
   readable by the bridge, and not include it; the bridge checks at
   startup. Here it decides little (who may push is linked writers, and
   the budgets exempt them), so any such group will do.
6. **Everyone the bridge acts as has signed in to Gerrit once**, so their
   account exists: a directory entry whose uid has no active account maps
   to nobody (§7).
7. The Checks tab plugin: `bridge --config … --checks_plugin >
   <site>/plugins/bridge-checks.js`.

### LDAP

The directory is the only mapping (§7): whoever it does not list is
nobody. Before the bridge reads it:

1. **The schema**: an auxiliary class `githubAccount` with MUST
   `githubLogin` (IA5, case-insensitive equality, single-valued) and
   `githubId` (integer, single-valued), indexed for equality, on the
   entries of `ou=People` (`inetOrgPerson`, `uid` = Gerrit username).
2. **Who may write them: administrators only.** An entry decides whose
   GitHub account is whose, and so who the bridge acts as, whose token a
   link stores, and whose Gerrit changes reach GitHub's Actions with
   secrets. The ACL must keep `uid`, `githubLogin`, `githubId` **and
   `objectClass`** (adding `githubAccount` to one's own entry) from being
   written by anyone but the directory's administrators: no `by self
   write` on them. (A writable `uid` is a Gerrit takeover at login
   anyway.) Check it: bound as an ordinary user, an `ldapmodify` of one's
   own `githubId` must be refused.
3. **Who may read them: anonymous.** The bridge binds anonymously and
   searches `(objectClass=githubAccount)` under the base DN for `uid`,
   `githubLogin` and `githubId`. An ACL that hides them from anonymous
   binds makes the search succeed with nothing usable; the bridge takes
   that as no answer (as if LDAP were down): nothing runs until a read
   finds an account it can use.
4. **The size limit** for anonymous searches (slapd's default is 500)
   must exceed the number of entries: a search that hits it is refused
   whole, and nobody is matched.
5. **The certificate**: its SAN must name `ldap.spacecookies.dev` (Go
   rejects a CN-only certificate); copy the CA (or the self-signed
   certificate itself) to the bridge's host for `ca_file`.
6. **Removing someone** is removing their `githubAccount` values (or the
   class) from their entry, or deactivating their Gerrit account. Locking
   their password or expiring their LDAP account does neither: the
   bridge reads only the GitHub attributes, and whether the Gerrit
   account is active, and Gerrit does not deactivate an LDAP account by
   itself. It stops acting as them within one refresh.
7. **Before saving a `githubId`**, the administrator checks it is the
   person's: a login that changed hands, or a mistyped one, gives a
   stranger their Gerrit account at once (comments, votes, uploads as
   them; and they can link). The LAM module looks the id up from the
   login as typed, at that moment.
8. **From the bridge's host**, the same search the bridge makes:
   ```
   LDAPTLS_CACERT=/etc/github-bridge/ldap-ca.pem ldapsearch -x \
     -H ldaps://ldap.spacecookies.dev:636 -b ou=People,dc=spacecookies,dc=dev \
     '(objectClass=githubAccount)' uid githubLogin githubId
   ```
   Every person, their uid, login and numeric id.

### GitHub

1. **The repository's merge settings** (§3.1): squash and merge commit
   messages are "Pull request title and description"; rebase merging off.
   Branch protection or rulesets as the team wants (reviews, checks), but
   the bot must still be able to push the bridge's `gerrit/*` and
   `gerrit-base/*` branches. A PR from any other branch GitHub protects
   (classic protection, or a ruleset's rule) is not imported, and the
   bridge never pushes there: Gerrit patch sets would reach it as the
   bot, past the rules (one protected after its import is withheld).
   A ruleset that targets every branch (`~ALL`) therefore stops every
   import from the repository's own branches: target the branches that
   need it.
   With "require conversation resolution", a thread resolved in Gerrit
   is resolved on GitHub only with its resolver's linked token, if GitHub
   lets them; otherwise the bridge reopens it in Gerrit, saying why, and
   never resolves it for them. A GitHub approval by whoever owns the
   change or uploaded the head's code is dismissed and casts no vote.
   A Gerrit-born change's PR is opened as its owner, with their linked
   GitHub account, so GitHub itself counts no approval of their own PR:
   until its owner has linked an account that may push, the change is
   withheld from GitHub, with a notice saying where to link.
2. **The bot's token**: Contents read & write (its branches), Pull
   requests and Issues read & write, Checks read (the Checks tab),
   Administration read (else the merge-settings check at startup only
   warns), Metadata read.
3. **The linking App** (`github.user_app`, §7.2): Pull requests and Issues
   read & write, and **Contents read & write** for `/submit` (which merges
   as the commenter); expiring user tokens on; installed on the
   repository; its callback URL `<public_url>/github/link/callback`.
4. **The webhook** to `<listen>/webhook`: content type JSON, the secret in
   `github.webhook_secret_file` (at least 32 characters), and these
   events: `pull_request`, `pull_request_review`,
   `pull_request_review_comment`, `pull_request_review_thread`,
   `issue_comment`, `push`. `push` matters here: without it Gerrit follows
   GitHub's merges only at the next resync.

### The proxy

1. **The Checks tab** is served under Gerrit's own path, so browsers send
   their Gerrit session: map `<gerrit>/<path>/checks/` to the bridge's
   `/checks/`, routed ahead of Gerrit's catch-all, forwarding the Cookie
   header. `ci.checks_url` is `<gerrit>/<path>`.
2. **List the proxy in `github.webhook_trusted_proxies`**, or every viewer
   shares the proxy's address and its per-client limits (the Checks tab
   polls every 30 s: 429s for the whole team). Only the proxy: whatever
   the list holds is believed when it says who sent a request, so a
   whole pod range is as safe as what keeps the rest of it out (a
   NetworkPolicy does not stop a node's own traffic to its pods).
3. **`public_url`** may be under a path (`https://gerrit.spacecookies.dev/bridge`):
   the link page posts and keeps its cookie under it. Its
   `/github/link` must reach the bridge's.
4. The bridge itself is reached only through the proxy: it `listen`s on
   loopback beside it, or (in Kubernetes) behind a NetworkPolicy that
   lets in only the proxy's pod. The status page (`status_listen`) stays
   on loopback either way. The bridge needs no Create on Gerrit's
   `refs/heads/*`: it follows only the branches Gerrit already has.

### The config

```yaml
branch_owner: github
gerrit:
  url: https://gerrit.spacecookies.dev
  project: software
  # http_user, http_password_file, mirror_dir, ssh: as example.yaml
github:
  owner: team1868
  repo: software
  webhook_secret_file: /etc/github-bridge/webhook-secret
  webhook_trusted_proxies: [127.0.0.1/32]
  token_file: /etc/github-bridge/github-token
  import_created_after: 2026-11-21T00:00:00Z   # else every open PR is imported at first start
  user_app:
    client_id: Iv23li...
    client_secret_file: /etc/github-bridge/user-app-secret
    public_url: https://gerrit.spacecookies.dev/bridge
    tokens_file: /var/lib/github-bridge/user-tokens
    key_file: /etc/github-bridge/user-tokens-key
identity:
  ldap:
    url: ldaps://ldap.spacecookies.dev:636
    ca_file: /etc/github-bridge/ldap-ca.pem
    base_dn: ou=People,dc=spacecookies,dc=dev
ci:
  checks_url: https://gerrit.spacecookies.dev/bridge
```

Nothing else under `identity`: `github_to_gerrit`, `match_usernames: true`
and `never_groups` are refused beside `ldap`, and the bridge will not
match usernames on a Gerrit that signs in with LDAP.

### Running it

1. **First with `--dry_run=true` (the default)**: it logs what it would
   do. Read the log: LDAP's entry count, the merge settings, who it would
   act as.
2. Then `--dry_run=false` under systemd with `Restart=always` and a
   backoff (`RestartSec=10`): it exits when Gerrit is unreachable for
   long, and on a bad config (there is no reload; `SIGHUP` stops it).
3. People link their GitHub accounts at `<public_url>/github/link`; until
   they have, their Gerrit changes stay out of GitHub (§3.1).
4. While LDAP cannot be read (including from startup until the first
   read, and a read that finds no account it can use), nothing runs; work
   waits and is tried again (§7). A lookup that cannot answer partway
   through (LDAP failing then, or Gerrit not saying whether an account is
   active) stops that work where it is, to be tried again: nothing is done
   for someone as if they were nobody. The log says so.

## Upgrading a Gerrit-owned deployment (AOS) from main

The config needs no change, except:
- **`ci.checks_url`** must be https (http only to a loopback address),
  with no query, fragment or credentials: the bridge will not start
  otherwise.
- The new keys (`branch_owner`, `identity.ldap`) default to what main did.

State (`refs/bridge/*`) gains fields (`found`, `vetted`,
`merged_notice_posted`), all optional: main reads them back (going back
loses them, and with them those fixes). A record main wrote of a Gerrit
patch set found on a GitHub-born PR's branch (no `found` then) is read
as found: one commit on both sides, and not `vetted`. An old import of
a commit that already carried its Change-Id looks the same, and is
looked at again (at worst, one notice).

Behaviour that changes, on purpose:
- **Gerrit patch sets pushed to a GitHub-born PR's branch** (a
  maintainer's on a contributor's PR) are now checked for the code below
  them too, as gerrit-buildkite checks it (§7.1): one on a stranger's
  change is not pushed, with a notice saying which. This reads up to 50
  commits below a patch set per look. Every such notice says the rule as
  it now is ("a patch set is pushed there only when whoever uploaded it,
  wrote its code and chose its parents is the change's owner or in CI's
  trusted group, and so is whoever did for the code below it"), not
  main's "only the change's owner, and members of CI's trusted group".
  An author's own change below their PR counts as theirs.
- A patch set Gerrit says was uploaded on someone's behalf, without
  saying by whom, is nobody's.
- A merged PR's reviews are no longer dismissed.
- **A PR merged on GitHub into another branch** (a stacked PR into the
  bridge's branch below it) is no longer reopened and retargeted over and
  over: the change stays open in Gerrit, and the PR says so once. A PR
  shown merged into the target branch (replication, whatever GitHub
  account it pushes with) gets nothing.
- **A PR GitHub shows merged by the bridge's own push** into a bridge
  branch (a chain reordered) says so once, on the PR and on the change,
  while the change is still open in Gerrit.
- **A Gerrit reply posted on someone's behalf** (another account's
  Run-As, or a vote with Label-As): its message, its comments and any
  thread it resolves are posted on GitHub as the bridge, naming both
  ("**Carol**, as **Alice**, ..."), never with the named person's own
  token nor as their GitHub review.
- **A linked user's +2 becomes their GitHub approval only while its
  commit is the PR's head, and only while it is still their vote** (no
  later Code-Review vote of theirs, and Gerrit still has it); otherwise
  its words are a comment naming the patch set.
- Maintain Server is neither needed nor asked about.

The Checks tab plugin keeps working; regenerating it is optional.
