# gerrit-github-bridge: Design

Status: draft, 2026-10-03. Target: Gerrit 3.14.4. Build: Bazel (bzlmod) + rules_go + gazelle.

## 1. Goal

One git repository, two review front-ends. A developer can upload and review
code through Gerrit or through GitHub, and everyone sees the same changes, the
same comments (on the same lines) and the same review state, whichever tool
they use.

Not goals (for v1):
- Multiple repos per change (Gerrit topics across repos).
- Fork-based GitHub PRs. GitHub stacks only work within one repo.
- Running CI on GitHub. Gerrit's CI is the only CI. Every GitHub push turns
  into a Gerrit patch set, so Gerrit's CI runs on it, and the results are
  mirrored back to the PR as check runs (§6.5).

## 2. Core model

| Concept              | Gerrit                                   | GitHub                                         |
|----------------------|------------------------------------------|------------------------------------------------|
| Unit of review       | Change (`Change-Id`, number)             | Pull request                                   |
| Revision             | Patch set N (commit SHA)                 | Head commit after each push/force-push         |
| Dependency chain     | Relation chain (parent commits)          | **Stack** (each PR's base = previous PR's head)|
| Per-unit diff        | Patch set vs. its parent commit          | PR head vs. base branch (= parent PR's head)   |
| Line comment         | `(ps, path, side, line \| range)`        | `(commit_id, path, side, line, start_line)`    |
| File comment         | comment with no line                     | review comment, `subject_type=file`            |
| Top-level comment    | patch-set message / `/PATCHSET_LEVEL`    | issue comment or review body                   |
| Threading            | `in_reply_to`                            | `in_reply_to` (review comments only)           |
| Resolution           | `unresolved` flag on latest comment      | review thread `isResolved` (GraphQL)           |
| Approval             | `Code-Review` label                      | Review `APPROVE` / `REQUEST_CHANGES`           |
| Verification         | `Verified` label                         | Check runs / commit statuses                   |
| Suggested edit       | `fix_suggestions`                        | ```` ```suggestion ```` block                  |
| WIP                  | Work-in-progress flag                    | Draft PR                                       |
| Abandon / restore    | Abandon / Restore                        | Close / Reopen                                 |
| Reviewer ask         | Reviewers + attention set                | Requested reviewers                            |

**Why stacks make this work.** Without stacks, a GitHub PR for change #3 in a
chain would either show the diff of #1+#2+#3, or target a branch that nobody
keeps current. In a GitHub stack, PR *k* targets PR *k-1*'s head branch, so
its diff is exactly Gerrit's patch set vs. its parent. Because the diffs are
the same, line numbers in comments carry over as-is in the common case.

**Same trees on both sides.** What has to match for comments to carry over
is the *diff*, not the commit. Each Gerrit patch set is tied to one GitHub
head commit, with these invariants:

- `tree(patchset) == tree(gh_head)`
- `tree(parent(patchset)) == tree(gh_base_head)`

So both sides show byte-identical diffs, even when the SHAs differ.

- Changes that start in **Gerrit** are pushed to GitHub as-is, so the SHAs
  match too. This is the simple case.
- Changes that start in **GitHub** are squashed on import (§5.2). The SHAs
  differ, the trees match, and a `revision_map` row links the two SHAs.

Anchoring always goes through `revision_map`. It never assumes the SHAs are
equal.

## 3. Source of truth

**Gerrit owns refs.** Gerrit is the only writer of protected branches
(`main`, release branches). The GitHub repo is a replica:

- Protected branches are copied Gerrit → GitHub (replication plugin or the
  bridge). On GitHub, branch protection lets only the bridge's App push there.
- Submit always happens in Gerrit. Pressing "Merge" on GitHub, including
  merging a stack through the async merge API, is turned into a Gerrit
  submit (§6.7). It is never a GitHub-side merge.

This removes the hardest problem: two systems both trying to advance `main`.
Review *conversation* is fully bidirectional. *Code* moves both ways, but it
only lands one way.

## 4. Architecture

```
            ┌──────────── bridge (single Go binary) ─────────────┐
 Gerrit ──► │ gerrit/ingest  ─┐                                  │
 stream-    │                 ├─► work queue ─► reconciler ──────┼──► Gerrit REST
 events /   │ github/ingest  ─┘   (keyed by      │   │           │
 webhooks   │  (App webhooks)      change)       │   └──────────┼──► GitHub REST/GraphQL
 GitHub ──► │                                    ▼               │
            │ poller (periodic full resync)   state (refs/bridge)│
            │                                 gitmirror (bare)  ─┼──► git fetch/push
            └────────────────────────────────────────────────────┘
```

### 4.1 Level-triggered reconciliation (the key design rule)

Events are only *hints*. Every event reduces to "reconcile the target
branch that change C is on". The whole branch is the unit, not one change
or one stack: a rebase can move a change between chains, so stacks can only
be planned with every chain on the branch in view. It costs one Gerrit
query and a few GitHub list calls per reconcile, and bursts of events
collapse into one reconcile. The reconciler then:

1. Fetches the current full state of C (and the rest of its chain) from
   **both** systems.
2. Loads the mapping state from `refs/bridge/changes/...`.
3. Works out the actions needed to make the two sides match, and applies them
   idempotently.
4. Records the new mappings in the same transaction it uses to mark the
   actions done.

This survives dropped webhooks, restarts, races and bridge downtime. A
periodic poller re-queues every open change, so missed events are
self-healing. The queue is de-duplicated per branch, and only one worker
handles a given branch at a time, so there are no concurrent edits to one
chain. (Routing a change's own `ref-updated` events to a per-change key
broke this in testing: two reconciles of one branch raced and each posted
the Gerrit link message. Change refs are now ignored; the change events
that accompany them carry the branch.)

Each reconcile plans (a pure function from a snapshot of both sides to an
ordered action list), applies, and re-plans until the plan is empty. New
PRs need one pass to get numbers before the next can stack them and fill
in the chain links, so it usually takes two or three passes.

### 4.2 Gerrit event stream

This is based on `AustinSchuh/gerrit-buildkite`, which already reads
`stream-events` for the same Gerrit. It runs
`ssh -o ServerAliveInterval=10 -o ServerAliveCountMax=3 … gerrit stream-events`
in a loop and decodes each line into an `EventInfo` struct. The bridge
keeps that shape, and fixes problems that would hurt a long-lived sync
service:

| gerrit-buildkite | Problem | Bridge |
|---|---|---|
| `json.Decoder.DisallowUnknownFields()` | Any new field Gerrit adds (it has added many in 3.x) makes the whole event fail to parse, and it is silently dropped. | Lenient decode. Read `type` first, then only the fields we use. Keep the raw JSON for debug logs. |
| `bufio.Scanner` with a 1 MiB limit | A big commit message or comment ends the scan with `ErrTooLong`, which kills the stream until it reconnects. | `bufio.Reader.ReadBytes('\n')` with no fixed limit (plus a large sanity cap that logs and skips). |
| Handles events inline on the read goroutine. A failed build trigger retries every 30 s **while blocking the read**. | Gerrit's per-client queue fills, and it sends `dropped-output` and discards events. | The read loop only parses and enqueues a key (the change's target branch) into the de-duplicating work queue. It never blocks on the network. |
| Reconnects right away in a tight loop | If Gerrit is down, the loop spins hot. | Exponential backoff with jitter, capped at 1 min. |
| No catch-up after a disconnect | Events sent while disconnected are lost. | Every (re)connect, and every `dropped-output` event, queues a full resync of open changes. Reconciliation is level-triggered, so that is all the recovery needed. |
| Subscribes to every event type | Volume. | `stream-events -s patchset-created -s comment-added -s change-abandoned -s change-restored -s change-merged -s change-deleted -s wip-state-changed -s private-state-changed -s reviewer-added -s reviewer-deleted -s vote-deleted -s hashtags-changed -s topic-changed -s ref-updated -s dropped-output`. |
| Shells out to `ssh` | Fine, and it reuses `~/.ssh/config`. | `golang.org/x/crypto/ssh` with `knownhosts`, protocol-level keepalives, and context cancellation. That keeps it one static binary with no subprocess to babysit. The same client runs `suexec` (§5.2) and `gerrit ls-members`. |

There is also a watchdog. If no event (including keepalives) arrives for N
minutes, the bridge reconnects and resyncs. A metric records the time since
the last event.

### 4.3 Packages

```
cmd/bridge/            main, flags, config loading
internal/config/       YAML config: repo pairs, label mapping, identity map
internal/gerrit/       REST client (changes, revisions, comments, review, submit),
                       event ingest (SSH `gerrit stream-events`)
internal/github/       GitHub App auth, REST (pulls, reviews, comments, stacks,
                       checks), GraphQL (threads, resolve/unresolve), webhook receiver
internal/gitmirror/    bare repo, shells out to `git` (fetch refs/changes/*, push PR branches)
internal/state/        per-change state in refs/bridge/* (read/CAS-write via gitmirror);
                       optional rebuildable cache
internal/model/        neutral types: Change, Revision, Thread, Comment, Anchor, Vote
internal/anchor/       comment position translation + porting across revisions
internal/markup/       Gerrit-markdown ⇄ GitHub-markdown, suggestion ⇄ fix_suggestion
internal/reconcile/    the diff/apply engine, split into sub-reconcilers:
                         code.go (revisions/branches/stack), comments.go,
                         votes.go, state.go (WIP/abandon/submit)
internal/queue/        keyed, de-duplicating work queue with retry/backoff
```

The sub-reconcilers work on `internal/model` types, not on Gerrit or GitHub
types. That way they can be tested against in-memory fakes of both sides.
Tests are mostly table-driven "given state A on Gerrit and B on GitHub,
expect actions X".

### 4.4 State storage: in the Gerrit repo, NoteDb-style

Goal: **nothing extra to back up**. If Gerrit's repos are backed up, the
bridge's state is too.

**Why not write into NoteDb itself.** NoteDb is the `refs/changes/XX/N/meta`
history that Gerrit owns. Gerrit caches what it parses from those refs, and
it validates the commit format. If anything else writes there, Gerrit ends
up with stale caches or a corrupted change. Data we put *through Gerrit's
API* (review messages, comment tags, hashtags) does land in NoteDb, but that
data is too coarse to rebuild the comment-ID mapping from.

**What we do instead.** Use the same mechanism, git refs inside the
project's own repository, under a namespace the bridge owns:

```
refs/bridge/changes/<NN>/<change-num>     one ref per change (sharded like NoteDb)
    state.json      change ↔ PR, branch, stack number, lifecycle state
    revisions.json  patchset ↔ gh head sha (+ tree hash for sanity checks)
    threads.json    gerrit root comment id ↔ gh thread node id / root comment id
    comments.json   gerrit comment id ↔ gh comment id, kind, origin, body_hash
    votes.json      account/label/value ↔ gh review id
refs/bridge/config                         identity map, label mapping overrides
```

- Each reconcile that changes a change's state makes one commit on that
  change's ref, pushed with `--force-with-lease`-style compare-and-swap. The
  history doubles as an audit log of what the bridge did and why.
- One ref per change means no write contention, and git's ref
  advertisement keeps lookups cheap. The same design reasons apply as for
  NoteDb.
- Gerrit ACL: the bridge account gets `Read`, `Push` and `Create Reference`
  on `refs/bridge/*`. Everyone else gets nothing (`Read` denied), so it stays
  hidden from users and from `git clone`.
- **Exclude `refs/bridge/*` from replication to GitHub.** It holds identity
  data.

**As built: the store caches in memory (2026-10-19).** A reconcile loads
the state of every change the bridge ever tracked on the branch (the
planner maps merged and abandoned changes' commits too), which on the
test instance was ~2,900 git commands, 3 of a reconcile's 4 seconds,
and would grow with history. Each change's state commit is kept in
memory from startup (`Fetch`) and moved by every save, including after a
conflict; files are cached by commit, which never changes. A load of a
change already seen runs no git command. So state must not be edited by
hand while the bridge runs (restart it after).

**SQLite (or nothing) as a disposable cache.** The work queue and webhook
dedup can live in memory. Reconciliation is level-triggered, so on startup
the bridge just re-queues every open change. If a local cache is useful for
speed (e.g. a GitHub comment id → change reverse index), it is rebuilt
from `refs/bridge/*` and can be deleted at any time.

**Secrets are the exception.** Per-user GitHub OAuth tokens (§7) must not
live in git. The options are encrypting them in `refs/bridge/config` with a
key from the bridge's environment, or keeping them in an external secret
store. If they are lost, users only have to re-link their accounts. No
mapping state is lost.

**Echo loops.** The bridge never mirrors a comment it wrote itself. It
checks both `origin` in `comments.json` and whether the author is the
bridge identity. As a backup, Gerrit reviews are tagged
`autogenerated:github-bridge` and GitHub bodies carry a hidden
`<!-- bridge:gerrit/12345/abcd -->` marker. Those markers also let the
mapping be partly rebuilt if `refs/bridge/*` is ever lost.

## 5. Code flow

### 5.1 Gerrit → GitHub

For each open, non-private change in a chain, bottom to top:

1. Fetch the current patch set into `gitmirror`.
2. Push it to `refs/heads/gerrit/<Change-Id>` on GitHub. The first patch
   set is Gerrit's commit itself. Later ones go on top of the PR's history
   (§5.2.1), so the PR keeps every patch set. This branch
   is bridge-owned, and the name stays the same across patch sets. One
   Change-Id can exist on several target branches (for example
   cherry-picks), so changes that target anything other than the default
   branch use `gerrit/<target-branch>/<Change-Id>`.
3. Make sure a PR exists with `head=gerrit/<Change-Id>` and
   `base=gerrit/<parent Change-Id>` (or the target branch for the bottom
   change).
   Title and body come from the commit message, without its `Change-Id`
   and `Signed-off-by` trailers. The PR body links back to Gerrit.
4. Make sure a GitHub stack exists (only for 2+ layers; see §5.4 for the
   API's quirks):
   `POST /repos/{o}/{r}/stacks {pull_requests:[bottom..top]}`. If the chain
   grew, use `/stacks/{n}/add`. If it was reordered or shrank, use
   `/unstack` and recreate it.
5. Record `revision_map` rows.

The push order is all branches first, then the PR/stack edits. Only one stack
is in flight at a time per chain.

**Non-linear chains.** Gerrit allows trees (two changes with the same
parent). GitHub stacks are linear, and a PR can belong to only one stack.
Rule: the bridge takes the longest linear path as the stack. Other branches
become PRs whose base branch is their parent's branch, but they are not
stack members. The bridge comments on them to explain this.

### 5.2 GitHub → Gerrit (squash on import)

A GitHub user builds a stack with `gh stack` or the UI, on branches in the
main repo. Each layer may contain any number of commits. The user never needs
a `Change-Id` or a commit-msg hook. The bridge turns each **layer into
exactly one Gerrit change**:

1. **Walk the stack bottom → top.** For layer *k*, with head commit `H_k`,
   find the layer's **real parent** `P_k`. Walk back through `H_k`'s first
   parents until reaching a commit that is either a known GitHub head of
   change k-1 (any patch set) or, for layer 0, a commit already on the
   target branch. (Not `merge-base(base, head)`: if the lower layer was
   force-pushed and this one wasn't restacked, the merge-base drops further
   down and would fold the lower layer's old patch set into this layer.)
   Then build one synthetic commit `S_k`:
   - `tree = tree(H_k)`
   - `parent` = the Gerrit patch set that maps to `P_k` (or `P_k` itself
     for layer 0). If that is an *older* patch set of change k-1, Gerrit
     shows "needs rebase", and the GitHub side is fixed up by the
     base-branch rule in §5.4.
   - If nothing matches (the base moved to a commit Gerrit has never seen),
     don't upload. Set check `gerrit/sync` to pending with "restack needed"
     and retry on the next push.
   - message: PR title + body, plus a `GitHub-PR: <url>` trailer and
     a `Change-Id` trailer, picked in this order:
     1. The Change-Id already recorded for this PR in `refs/bridge`. Once a
        PR is linked to a change, the link never changes.
     2. A `Change-Id` trailer already on the layer's commits. This is what
        happens when the GitHub user has Gerrit's commit-msg hook installed,
        which we recommend (see §5.4). If the commits carry different
        Change-Ids, use the bottom commit's ID and warn in the
        `gerrit/sync` check.
     3. Otherwise, `I<sha1(repo, PR node id)>`. This is deterministic.
     If that Change-Id already belongs to a *different* PR or to a change
     the bridge didn't create, refuse and explain in `gerrit/sync`. That
     prevents two PRs from fighting over one change.
   - author: taken from `H_k`, which is the person who wrote the code.
     Committer: the Gerrit account the push runs as (step 2). Gerrit
     rejects a push whose committer email is not one of the uploader's
     emails unless the uploader has Forge Committer, and with `suexec` the
     uploader is the user, not the bridge.
   - **No-op squash:** if the layer is exactly one commit, that commit
     already has the chosen Change-Id, its parent is the Gerrit commit
     chosen above, and its committer email belongs to the pushing Gerrit
     account, then `S_k = H_k`. Nothing is rewritten, and the SHAs
     match on both sides. Users with the commit-msg hook and one commit per
     layer always end up here.
   - author/committer dates: taken from `H_k`. **Squashing is deterministic.**
     The same GitHub state always produces the same `S_k` SHA, so retries
     never create extra patch sets ("no new changes" is a no-op for Gerrit).
2. **Push the chain in one go, as the right owner.** The bridge runs
   `git push gerrit S_top:refs/for/<target>` with `%topic=`, `%wip`/`%ready`
   and reviewer options. Gerrit creates or updates one change per `S_k`,
   and the relation chain matches the stack.

   The account that uploads the first patch set becomes the change's
   **owner**, and Gerrit has no REST endpoint to change the owner later. So
   the push must be done *as the GitHub user's mapped Gerrit account*:
   - Gerrit's SSH `suexec` command runs `git-receive-pack` as another user.
     It is only open to keys listed in `$site_path/etc/peer_keys`, so the
     bridge gets one of those. The bridge then runs
     `git push --receive-pack='suexec --as <account> -- git-receive-pack'
     'ssh://Gerrit%20Code%20Review@gerrit:29418/<project>' …`.
     **Verified on 3.14.4** (`integration/gerrit_test.go`,
     `TestSuexecPushOwnership`): the change's owner and uploader are
     `<account>`. Details that matter:
     - The SSH user name must be exactly `Gerrit Code Review`.
     - `peer_keys` is not `authorized_keys` format. Each line is the
       base64 key blob plus an optional *one-word* comment, with no key
       type. A line with more fields makes Gerrit silently ignore the
       whole file. Gerrit picks up changes to the file without a restart.
     - The peer user can do nothing except `suexec`. A plain fetch as
       the peer hangs instead of failing, so fetches go through `suexec`
       too, or use the bridge's own account.
     - `suexec` can act as *any* account, administrators included. The
       peer key is the most sensitive secret the bridge holds, so use a
       separate key from the bridge account's SSH key.
   - The patch set uploader is that user too. Later patch sets imported
     from GitHub are pushed as whoever pushed to GitHub (the `sender` of the
     push event, mapped).
   - If the GitHub user has no Gerrit account: the bridge account owns the
     change, the author is still the real person, and the PR author is added
     as a CC once a mapping exists. The `gerrit/sync` check says that
     linking an account would fix ownership.
3. **Record** `revisions.json`: `(patchset, S_k)` ↔ `H_k`, with
   `tree(S_k) == tree(H_k)` checked before every write.

**Gerrit → GitHub for a GitHub-born change.** Someone uploads a new patch set
in Gerrit (an edit, "Apply fix", or a rebase):

- **Same parent state** (content changed only): append a commit to the
  user's branch with `tree = tree(new PS)` and message
  "Patch set N from Gerrit (Jane Doe)". This is a fast-forward, so the user's
  local branch history is preserved and `git pull` just works.
- **Parent changed** (rebased in Gerrit): append a merge commit "Patch
  set N from Gerrit, rebased" of the branch and the new parent's GitHub
  commit, with the patch set's tree, as for Gerrit-born changes (§5.2.1).
  Still a fast-forward, so the history and `git pull` keep working. (Until
  2026-10-18 this force-pushed the Gerrit commit itself.)

**Changes that start in Gerrit** keep the simple path from §5.1. The SHAs
are identical and nothing is squashed.

### 5.2.1 What a force-push does to GitHub comments

Every new Gerrit patch set ends up as a push to the PR branch. That push is
usually a force-push: Gerrit patch sets are amends, not commits added on
top. GitHub does not drop comments when this happens. Each comment keeps
`original_commit_id`/`original_line`, and GitHub re-maps it onto the new
head when it can. When the code under a comment changes, the thread is
marked **outdated**. It is then still shown in the Conversation tab, but
hidden from Files changed. Gerrit does the same thing: comments stay on
the old patch set and are "ported" forward.

**Measured on 2026-10-03** with `experiments/force_push_comments.sh`, on a
native stack of two PRs (`AustinSchuh/bridge-scratch`):

| Case | What was pushed | Result |
|------|-----------------|--------|
| A | New PS for layer 2, commented lines untouched | Nothing outdated. The comments' `commit` moved to the new head; `line` unchanged. |
| B | New PS for layer 2 that edits commented line 32 | Only that thread went `isOutdated`, with `line=null`, and it stays pinned to the last commit where it was valid. The line-31 comment followed the head. |
| C | New PS for layer 1, layer 2 rebased onto it (layer 2's diff unchanged) | Nothing outdated in either PR. The layer-1 comment followed the new layer-1 head too. |
| D | Comment posted with `commit_id` of a force-pushed-away commit (single-comment and review APIs) | **Accepted.** GitHub re-mapped the comments onto the current head right away and did not mark them outdated (that line was unchanged). A commit/line that is not in the PR's diff is rejected with `422 pull_request_review_thread.line could not be resolved`. |

Conclusions:
- A force-push only outdates comments on lines that actually changed. That
  matches what Gerrit does when it ports comments, and it is the same as a
  native restack. The bridge does not have to do anything special.
- A pure rebase of a stack layer does not disturb comments, so restacking
  after a lower layer changes is free.
- A Gerrit comment on an old patch set can be posted to GitHub against
  that patch set's commit, so §6.1 step 5 needs no porting fallback in the
  common case. Porting is still needed when the anchor is not in the old
  commit's diff hunks; the bridge falls back to file-level then.
- Not yet tested: posting against an old commit whose line *has* since
  changed (it is expected to show up as outdated straight away).

Appending a commit instead of force-pushing (§5.2) changes nothing about
where comments land, because GitHub re-maps against the PR diff either
way. It keeps the history, though, and that is what reviewers on GitHub
use: "changes since your last review", the per-commit views, and the
commits their comments were written against. So since 2026-10-17 every
new patch set of a Gerrit-born change goes on top of its PR's head too:

- **Same parent** (an amend): a commit "Patch set N from Gerrit" with the
  patch set's tree, on top of the head.
- **New parent** (rebased in Gerrit, or the change below has a new patch
  set): a merge commit "Patch set N from Gerrit, rebased" of the head and
  the new parent's GitHub commit, with the patch set's tree. The PR's diff
  against its base is still exactly the patch set's own change.
- **Same tree and parent** (say, a commit-message edit): nothing is pushed.

The GitHub commits then differ from Gerrit's, as for GitHub-born changes,
and `revisions.json` maps them. At submit the bridge swaps each PR's head
for Gerrit's commit (§6.7), so GitHub can still show it merged.

### 5.2.2 Pushes to a Gerrit-born PR

It should not matter where a change started. Anyone may push to a
Gerrit-born PR's `gerrit/<Change-Id>` branch, by hand or with GitHub's
buttons ("Rebase stack", "Update branch", committed suggestions). The bridge
imports the push like one to a GitHub-born PR (§5.2): one squashed patch set
of the existing change, uploaded as §7.1 says (the pusher if they
uploaded the current patch set, its uploader for a pure rebase, otherwise
the bridge). The change keeps its
owner, Change-Id, commit message and WIP state, which belong to Gerrit. A
layer that merged its base ("Update branch") is squashed onto what it
merged, so its diff stays its own. A stack rebase imports each layer in
order. Until the push is imported, the bridge never pushes over it; if it
cannot be imported, `gerrit/sync` says why.

### 5.3 Conflicts in code

For each change, the stored state includes the last synced `(patchset, gh
head)` pair. On reconcile:

- Only GitHub moved → import (§5.2).
- Only Gerrit moved → export (§5.1, or the append/force-push rule above).
- **Both moved** → Gerrit wins. The new GitHub head is imported as an
  extra patch set *first* (so nothing is lost and it stays visible in
  Gerrit's history), then Gerrit's newest patch set is pushed back. A notice
  is posted on both sides. This should be rare, and the rule has to be
  deterministic, not clever.

### 5.4 Round trips, partial pushes and reparenting

Everything here follows from two rules:

**Rule 1: sameness means the same tree and the same parent tree, not the same
SHA.** When the reconciler compares a Gerrit patch set with a GitHub head,
it counts them as the same revision if `tree` and `tree(parent)` both
match. It then only records the mapping and pushes nothing. Without this, a
commit that reached both sides by different routes would bounce back and
forth forever.

**Rule 2: on GitHub, a PR is always diffed against the same commit Gerrit
diffs it against, even if that means taking it out of the stack.**

The problem, as an example. Gerrit has a chain A ← B ← C, and GitHub
mirrors it as a stack. Each PR's base is the branch of the PR below it:

```
Gerrit                         GitHub (stack)
  C   parent = B1                PR-C   base: gerrit/B
  B1  parent = A                 PR-B   base: gerrit/A
  A   parent = main              PR-A   base: main
```

Now someone amends B into B2 and pushes **only B** (not C). Gerrit is
fine with that:
- C keeps its parent B1, and Gerrit shows "needs rebase".
- Gerrit still diffs C against B1, so reviewers see only C's own edits.

The bridge moves branch `gerrit/B` to B2. On GitHub, PR-C's base is
`gerrit/B` (now B2), and GitHub diffs a PR against
`merge-base(base, head)`. B2 and C only share history back at A, so
**PR-C now shows B1's changes as well as C's**:
- Reviewers on GitHub see a different diff than reviewers on Gerrit.
- Comments on the B1 lines inside PR-C have no place to go in Gerrit.
- Native GitHub behaves the same way when a lower layer is pushed without
  restacking. The difference is that the bridge has to keep both diffs
  equal.

The fix. The bridge creates a branch `gerrit-base/<C's Change-Id>`
pointing **at B1**, and changes PR-C's base to it:

```
Gerrit                         GitHub
  C   parent = B1                PR-C   base: gerrit-base/C (= B1)  ← own stack
  B2  parent = A                 PR-B   base: gerrit/A    ┐ stack
  A   parent = main              PR-A   base: main        ┘
```

- GitHub now diffs PR-C against B1, exactly as Gerrit does.
- The cost: PR-C has to **leave the stack**. GitHub only allows PRs in one
  stack when each PR's base is the head branch of the PR below it, and
  `gerrit-base/C` isn't PR-B's head branch. So the stack becomes [A, B],
  and C, with anything above it, forms its own stack.
- The bridge comments on PR-C: "Based on an outdated version of PR-B.
  Rebase in Gerrit or run `gh stack rebase`."
- When someone rebases C onto B2, from either side, PR-C's base goes back
  to `gerrit/B`, the stack becomes [A, B, C] again, and `gerrit-base/C` is
  deleted.

**What splitting or rejoining a stack costs** (measured 2026-10-03 on
`bridge-scratch`: unstack, retarget a PR's base, and recreate the stack):
- **Kept:** all review comments and threads, their resolved state, their
  outdated state and line anchors, review history, and the PRs themselves.
  None of that belongs to the stack.
- **Changed:** the stack gets a **new number**. Stack #5 became #6, and
  links to the old stack stop working. The bridge never treats a stack
  number as an identity. It stores it only to know which stack to unstack,
  and the cross-link block (§6.0) shows the current one.
- **Timeline noise:** each affected PR gets `removed_from_stack`,
  `base_ref_changed` and `added_to_stack` entries. These are timeline events
  only. They are not comments and don't email reviewers.
- **Approvals (the real cost):** GitHub's "dismiss stale approvals" rule
  dismisses an approval whenever a PR's *merge base* changes. Splitting
  changes PR-C's merge base, and so does the earlier push of B2. So
  GitHub-side approvals on the affected PR are dismissed. The design
  absorbs this:
  - GitHub approvals don't gate anything. Gerrit votes do, and they reach
    GitHub as the `gerrit/submittable` check (§6.7). So the mirrored repo
    should **not** enable GitHub's required reviews or "dismiss stale
    approvals". `gerrit/submittable` is the only required gate.
  - If dismissal is enabled anyway, the bridge ignores dismissals made by
    GitHub itself (`review_dismissed` with no human actor). It does not
    remove the matching Gerrit vote. Gerrit's own `copyCondition` decides
    whether a vote survives a rebase.
  - Approvals the bridge mirrored from Gerrit are re-posted if the Gerrit
    vote is still there.

The general form: for every PR, if its head's parent is the current head
of the PR below it, the PR stays in the stack. Otherwise it gets a frozen
`gerrit-base/…` branch at its real parent and starts a new stack.

Frozen bases are not named `gerrit/<Change-Id>/base`: git refs cannot be
both a file and a directory, so that branch could never coexist with
`gerrit/<Change-Id>`.

**Verified live (2026-10-04)** with a 3-change chain mirrored from the test
Gerrit to `bridge-scratch` (PRs #7, #8, #9): amending only the middle change
moved PR #9 to `gerrit-base/…` (diff still only its own file) and the
stack became [#7, #8]; rebasing it rejoined the stack through
`/stacks/{n}/add` and deleted the frozen branch; abandoning the middle
change closed its PR with one comment and froze the layer above;
restoring it reopened the PR and rebuilt the stack; WIP toggled draft; a
fourth layer was added to the existing stack.

The alternative would be to leave PR-C in the stack and accept the bloated
diff, as native GitHub does. That is rejected because comments made on the
extra lines could not be mirrored to Gerrit.

- Verified on 2026-10-03:
  - `/unstack` takes no body and **dissolves the whole stack**
    (`204`). Splitting a stack therefore means unstacking it and creating
    the two halves again.
  - A stack needs **at least 2 PRs**. A single layer left on its own is a
    plain PR with no stack.
  - A stack's bottom PR may target any branch, including a frozen base:
    `[PR2 → frozen-base, PR4 → layer2]` was accepted.

The scenarios:

- **Push to GitHub → sync → fetch from Gerrit → amend → push to Gerrit.**
  The fetched commit carries the change's Change-Id (from the squash
  message or the user's own trailer), so Gerrit treats the push as a new
  patch set of the same change. The bridge sees that only Gerrit moved and
  exports it to the PR branch (fast-forward or force-push, §5.2). If the
  user pushed something Rule 1 counts as the same revision, nothing
  happens.
- **Push to Gerrit → sync → pull the GitHub branch → amend → push to
  GitHub.**
  - To the same branch (`gerrit/<Change-Id>`, which collaborators may push
    to, or the user's own branch): GitHub moved, so the bridge imports a
    new patch set, with the bridge's own message (it never uploads the
    PR's commit itself, so the SHAs differ even for one commit with a
    Change-Id).
  - To a *new* branch with a new PR carrying the same Change-Id: the
    Change-Id is already linked to another PR, so the bridge refuses and
    points at the existing PR (§5.2 step 1).
  - **Recommendation:** everyone installs Gerrit's commit-msg hook. Then
    every commit has its Change-Id from the start, so a commit has the same
    identity whichever way it travelled, and most of these cases become
    no-ops.
- **Both moved between two reconciles:** see §5.3. Gerrit wins, and the
  GitHub revision is kept as an extra patch set.
- **Partial chain push in Gerrit** (amend change 2 of 4, push only up to 2):
  changes 3 and 4 are still on patch set 1 of change 2, which Gerrit shows
  as "needs rebase".
  - GitHub: PR2's branch moves to patch set 2.
  - PR3 moves to a frozen base `gerrit-base/<C3>` (pointing at C2 patch
    set 1) under Rule 2, so its diff stays exactly C3.
  - The stack splits into [PR1, PR2] and [PR3, PR4].
  - The bridge does **not** rebase anything on its own. Gerrit users
    expect "needs rebase" to stay until they act.
- **Partial push on GitHub** (force-push layer 2 without restacking): the
  mirror image. The bridge imports patch set 2 of C2. Under §5.2's
  real-parent walk, C3's parent is still patch set 1, Gerrit shows "needs
  rebase", and Rule 2 splits the GitHub stack to match.
- **Reparenting or reordering** (in Gerrit via rebase/"edit parent", or on
  GitHub via `gh stack` reorder): reconciliation is level-triggered, so the
  bridge recomputes the linear stacks from the current patch sets' parents
  and applies the difference in a fixed order:
  1. push the head branches and frozen base branches;
  2. `/unstack` the stacks that changed;
  3. retarget PR bases (`PATCH /pulls/{n} {base}`);
  4. create the new stacks;
  5. delete frozen branches nothing uses any more.
  A change that is moved out of a chain (rebased onto the target branch)
  becomes a stack of its own. A change moved between chains joins the
  other chain's stack.
- **Abandoning a change in the middle of a chain:** Gerrit leaves its
  children on the abandoned commit. Rule 2 handles this: the children move
  to a frozen base, and the closed PR leaves the stack.

## 6. Review flow

### 6.0 Cross-links

Each side always links to the other, so users can find it.

- **GitHub PR body:** a block the bridge owns, between
  `<!-- bridge:links -->` markers. It holds the Gerrit relation chain, top
  first as in Gerrit and shown even for a single change. Each entry links
  to its Gerrit change and its PR, and this PR's own entry links to the
  current patch set.
- **PR title and description = the commit message, both ways, wherever
  the change started.** The title is the subject, and the description
  (above the block) is the rest, without the `Change-Id`, `Signed-off-by`
  and `GitHub-PR` trailers. Whichever side changed since they last agreed
  (`synced_text`) wins: a GitHub edit becomes a new patch set with that
  commit message (`PUT /changes/N/message`, same files), made as the
  editor through Run-As, or as the bridge for someone without a Gerrit
  account, as pushes are; the trailers are kept. If Gerrit refuses, or
  both sides changed, a notice says so and Gerrit's version goes back on
  the PR. Draft and WIP work the same way (`synced_wip`): toggling either
  sets the other, as the person who did it. Later imported patch sets keep
  the change's commit message, adding the new commits' sign-offs.
- **GitHub checks:** every `gerrit/*` check run's `details_url` points at
  the Gerrit change, so it is one click from the merge box.
- **Gerrit change:**
  - When a PR is first linked (or re-linked), the bridge posts a change
    message, "GitHub: <PR url> (stack #S)". It is tagged
    `autogenerated:github-bridge:link`, so the change log shows it but it
    can be filtered out.
  - GitHub-born changes also carry the `GitHub-PR: <url>` commit-message
    trailer, and the project's `commentlink` config turns it into a link.
  - Later: a small Gerrit UI plugin that shows a "GitHub PR" chip in the
    change header, fed by the bridge's `GET /links/<change>` endpoint. That
    way Gerrit-born changes get a permanent link too, without a commit
    message edit.
- **Each mirrored comment** ends with a small "view on Gerrit / view on
  GitHub" link that goes to the original comment.

### 6.1 Anchoring comments (`internal/anchor`)

Translating a Gerrit comment `(patchset P, path, side, range)` into GitHub:

1. `commit_id = revisions[P].gh_head`. For Gerrit-born changes this is
   the same SHA. For GitHub-born changes it is the squashed-from head. In
   both cases the trees match, so the line numbers do too.
2. `side`: Gerrit `REVISION` → `RIGHT`; Gerrit `PARENT` → `LEFT`.
3. Range: `start_line = range.start_line`, `line = range.end_line`. GitHub
   has no column precision, so the exact character range is kept in the
   hidden marker, and the selected text is quoted in the body when the range
   covers part of a line.
4. **Diff-hunk constraint.** GitHub only accepts line comments on lines that
   appear in the PR's diff (changed lines plus context). Gerrit accepts
   comments on any line. If the line is outside the hunks, post a
   *file-level* comment (`subject_type=file`) that starts with
   `` `path:L120-L124` `` and a quoted snippet. The mapping remembers the true
   anchor, so a reply from GitHub goes back onto the right Gerrit line.
5. **Old revisions.** If P is no longer the PR head, post with
   `commit_id = old sha`. GitHub accepts force-pushed-away commits and
   re-maps the comment onto the head if the line still exists (verified in
   §5.2.1, case D). Only if that fails with a 422, port the anchor to the
   latest revision (step 6) and add "(from patch set P)".
6. **Porting.** Line mapping between two revisions of the same file uses a
   diff of the two blobs, the same approach as Gerrit's "ported comments". If
   the line was deleted, fall back to a file-level comment.
7. `/COMMIT_MSG` comments become PR-level comments that quote the commit
   message lines. GitHub has no commit-message anchor.
8. `/PATCHSET_LEVEL` comments become a review body (if they came with a vote)
   or an issue comment.

Going GitHub → Gerrit is the same process in reverse. `commit_id` → patch
set; `LEFT` → `PARENT`; GitHub's `original_commit_id`/`original_line` is used
for outdated comments, so the comment lands on the patch set it was written
against.

**Comments on intermediate commits** (GitHub-born, multi-commit layers):
a reviewer may comment in GitHub's per-commit view on a commit `C` that is
not a layer head. Gerrit never saw `C`, so the anchor is *ported* (step 6)
from `C`'s blob to the patch set built from the nearest later head that
contains `C`. If the line doesn't survive, the comment falls back to
file-level and gets a note "(on commit abc1234)".

### 6.2 Threads, replies, resolution

- Each GitHub review thread maps to one Gerrit comment thread (the root plus
  its `in_reply_to` chain). Replies are mirrored as replies.
- Gerrit has no thread object. "Resolved" is the `unresolved` flag of the
  last comment. GitHub has an explicit `resolveReviewThread` mutation. The
  rules:
  - Gerrit reply with `unresolved=false` → GitHub `resolveReviewThread`.
  - GitHub resolve → Gerrit reply "Done" (empty or short body) with
    `unresolved=false`, authored as that user if possible.
  - The two unresolve cases mirror these.
- PR issue comments have no threads. A top-level Gerrit message is posted as
  an issue comment, and an issue comment becomes a patch-set-level comment.

### 6.3 Batching

Gerrit publishes a reply as one atomic `SetReview` (message, comments and
votes together). GitHub has the same shape: a *review* (body, comments and
event). The bridge keeps them as single units:
Gerrit `SetReview` ⇄ GitHub `POST /pulls/{n}/reviews` with all comments
attached. One notification per side instead of N.

Exception: a standalone GitHub reply in an existing thread
(`POST /pulls/{n}/comments/{id}/replies`) becomes a one-comment `SetReview`.

**Attribution and the comment map (GitHub → Gerrit).** A comment posted
as its author (Run-As) is just their text: Gerrit already shows who wrote
it. One the bridge posts under its own account (an unlinked user, an
edited comment, or Run-As refused) starts with `**@login**
([GitHub](<comment URL>)):`. Either way the bridge then finds its Gerrit
copy by what it posted: a comment by the bridge (or by the user, tagged
`github-bridge:mirror`), not mapped yet, at the same patch set, file, line,
range, side and parent, with the same text; identical comments pair up
oldest first. That is how a new comment enters `comments.json`, and why a
crash between posting and saving it does not post it twice, so
`omit_duplicate_comments` is not used (it would drop a second, separate
"Done"). The header's link is also read back, but only from the bridge's
own posts: a user's text may imitate the header.

### 6.4 Votes

Configurable per label. The default:

| Gerrit                 | GitHub                                               |
|------------------------|------------------------------------------------------|
| Code-Review +2         | Review `APPROVE` (by a mapped user with write access)|
| Code-Review +1         | Review `COMMENT` with "+1" (GitHub has no weak approve)|
| Code-Review -1         | Review `COMMENT` with "-1"                           |
| Code-Review -2         | Review `REQUEST_CHANGES`                             |
| Vote removed           | Dismiss review                                       |

GitHub → Gerrit: `APPROVE` → +2 if the user has the right in Gerrit,
otherwise +1. `REQUEST_CHANGES` → -1 (configurable to -2). Dismissal → vote
removed.

A GitHub approval on a stack layer applies only to that layer's Gerrit
change. This matches the semantics on both sides.

GitHub shows what Gerrit counts: when a reviewer's Code-Review vote no
longer agrees with their GitHub approval or change request (they changed
it in Gerrit, a new patch set reset it, or it was never cast), the bridge
dismisses the review, saying what the vote is now. Only reviews already
mirrored are checked, and only for reviewers with a Gerrit account.

### 6.5 CI

**GitHub follows Gerrit.** Gerrit's CI is the only CI.

- Every GitHub push becomes a Gerrit patch set (§5.2). Gerrit's CI triggers
  on that patch set exactly as it would for a `git push` to Gerrit. Nothing
  on the GitHub side starts CI.
- The bridge mirrors the results back to the PR head commit as GitHub check
  runs. It uses `revisions.json`, so this works for squashed GitHub-born
  revisions too.
  - Each CI label (`Verified`, plus any configured extras) becomes a check
    run `gerrit/<Label>`. Its state comes from the vote: queued/in-progress
    while there is no vote on the current patch set, success for +1, and
    failure for -1. `details_url` points at the Gerrit change.
  - Messages from the CI account (by `tag`, e.g. `autogenerated:ci`) become
    the check run's summary text, with its links.
  - The overall submit requirement result becomes check run
    `gerrit/submittable` (§6.7).
- GitHub branch protection marks `gerrit/Verified` and `gerrit/submittable`
  as required. That way the GitHub merge box shows the same gate Gerrit
  enforces.
- GitHub Actions workflows that gate code should be turned off in the
  mirrored repo, or treated as advisory only. Their results are **not**
  copied to Gerrit.
- **Trust reuses gerrit-buildkite's model, unchanged.** gerrit-buildkite
  starts a build on `patchset-created` only if the event's **uploader** is
  in the Gerrit group `Verified Users` (`ls-members --recursive`). A
  `comment-added` whose body has a line that is exactly `retest` also
  starts a build, but only if its **author** is in that group. The bridge
  works with this rather than around it:
  - Imports are pushed *as the mapped Gerrit user* (`suexec`, §5.2 step 2),
    so the uploader in `patchset-created` is the real person.
    gerrit-buildkite's check then applies to GitHub users exactly as it
    does to Gerrit users. No CI-side changes are needed.
  - **The bridge account must not be in `Verified Users`.** When a GitHub
    user has no Gerrit mapping, the bridge uploads as itself. Leaving it
    out of the group means unrecognized pushes never start CI.
  - A recognized user can start or retry CI from GitHub by commenting
    `retest`. The comment is mirrored with Run-As (§7), on the patch set
    GitHub had when they wrote it (§7.1), so Gerrit sees them as the
    author and gerrit-buildkite builds that patch set. Posted as its author,
    the comment is just their text, so `retest` stays a line of its own.
  - The bridge reads the same group (REST
    `GET /groups/{id}/members?recursive`, cached briefly) so GitHub can
    explain what's going on. When the uploader isn't recognized,
    `gerrit/Verified` stays *queued* with the summary "CI not started:
    uploader is not in Verified Users. A trusted reviewer can comment
    `retest`."
  - The group name is a config value (`ci.trusted_group`, default
    `Verified Users`).
- **CI results come from Buildkite, not from CI's messages**
  (`ci.buildkite`; `internal/buildkite`). gerrit-buildkite builds a patch
  set with `commit` = the Gerrit revision, so the bridge lists the
  pipeline's builds of that commit (`GET …/builds?commit=`, a read_builds
  token). It reads them whenever it syncs a change, which CI's own
  "Build Started" and vote messages trigger.
  - `gerrit/Verified` on GitHub: while there is no vote, the newest
    build's state ("CI running", "CI failed; waiting for the vote"), with
    `target_url` = the build. The vote still decides success or failure.
  - **Gerrit's Checks tab** (`internal/gerritchecks`). A small UI plugin,
    `plugins/bridge-checks.js` (`bridge --checks_plugin` writes it), is a
    ChecksProvider: for each patch set it fetches
    `GET <ci.checks_url>/checks/<change>/<patch set>` from the bridge,
    which answers with Gerrit `CheckRun`s: one per Buildkite job (or per
    build before it has jobs), each retry of the build a new attempt,
    running/scheduled/completed, with a result (passed, failed with exit
    status, canceled, not run) linking the job. Its **Rerun** action posts
    `retest` as the signed-in user, so gerrit-buildkite's own trust check
    decides, exactly as if they had typed it. The plugin polls every 30s.
  - The browser cannot call Buildkite (that needs the token), hence the
    bridge in the middle. `/checks` is public: it shows only changes in
    the bridge's project that anonymous users can see (it reads the change
    without credentials), caches each answer 15s, and computes at most 4
    at once. It sends CORS headers for Gerrit's origin, for when the
    bridge is not proxied under Gerrit's host.
- Without `ci.buildkite`, the older fallback applies: messages from
  `ci.account` matching `ci.message_pattern` ("Build
  Started|Succeeded|Failed: <url>") give `gerrit/Verified`'s text and link,
  and there is no Checks tab.

### 6.6 Lifecycle

| Event                     | Mirror                                          |
|---------------------------|-------------------------------------------------|
| WIP on/off                | Draft / ready for review, both ways (§6.0)      |
| Abandon / Restore         | Close / Reopen (stack: `/unstack` that layer); and back: closing or reopening the PR abandons or restores the change as whoever did it, if Gerrit lets them (else the PR is put back, with a notice). Administrate Server does not carry through Run-As, so administrators need an explicit Abandon grant (`refs/*`, All-Projects) to abandon others' changes this way |
| Private change            | **Never mirrored**                              |
| Reviewer added / attention| Request review (mapped users only)              |
| Hashtags                  | Labels (optional)                               |
| Commit message edit (new PS) | PR title/body updated, and back: a PR edit is a commit message edit (§6.0) |

### 6.7 Submit and merge

- **Gerrit submit** (single change or the whole chain): Gerrit updates
  `main`, and replication copies it to GitHub. If submit used
  fast-forward/merge, GitHub sees the PR head SHAs in `main` and
  auto-detects the merge. Otherwise the PR heads are not the merged commits
  (rebase/cherry-pick submits, and PRs with patch-set history). For
  Gerrit-born PRs the bridge swaps the heads in (next point). Other PRs are
  closed with "Merged in Gerrit as change N.". The `gerrit/<Change-Id>`
  branches are then deleted, and GitHub retargets the remaining stack
  layers.
- **Showing the PRs as merged (measured 2026-10-11).** GitHub marks a PR
  merged only when its head commit arrives on the PR's **base** branch. A
  stacked PR is based on the layer below, so a pushed-in chain leaves every
  layer but the bottom open (native stacks do not retarget on push merges,
  even after a minute), and once the commits are in, the PR can no longer
  be retargeted ("no new commits"). So when Gerrit reports a change merged
  and the PR's head is exactly the merged commit, the bridge immediately
  takes the PR out of its stack and retargets it to the target branch.
  A PR whose head is not the merged commit first has its head swapped for
  it, a force-push of the same files: a Gerrit-born PR's patch-set history
  (§5.2.1), a GitHub-born PR's commits that Gerrit squashed (the author's
  branch; a fork only if it allows maintainer edits), and submits that
  rebased the commit. `/submit` does both before submitting.
  Replication then lands the commits and GitHub shows it merged. The
  bridge hears the merge event within a second, while replication waits
  `replicationDelay` (default 15 s; AOS uses the default). A full
  reconcile of the branch can take longer than that (many open PRs,
  comment syncs queued by a resync), so a `change-merged` event queues a
  `merged/N` key that does only the swap and retarget (`PrepareSubmit`),
  goes to the next free worker ahead of everything else, and takes the
  branch lock ahead of anyone else waiting for it; the full reconcile
  follows (audit follow-up 2026-10-19; slow or late reconciles are logged).
  If replication wins anyway, or the
  submit rewrote the commit (rebase, cherry-pick, any GitHub-born squash
  that is not a no-op), the PR is closed with a "Merged in Gerrit" notice.
  That happens when replication wins, or for a fork without maintainer
  edits.
  A PR's head branch is never deleted while the PR is open (that would
  close it unmerged). Verified with `tools/testenv/rehearsal.sh`.
- **GitHub merge button**: branch protection lets only the bridge App push
  to `main`, so a GitHub merge cannot happen directly. Without it, anyone
  with write access could merge on GitHub; Gerrit would never see it, and
  replication (`force`) would then erase it from `main` while the PR stays
  "merged". The test instance does it with a ruleset on `main` (no updates,
  force pushes or deletion; `gerrit/submittable` required) that only
  replication's deploy key bypasses (`docs/test-instance.md`). For AOS
  (an organization's public repo) the same ruleset can name the
  replication account or App as its only bypass. Instead:
  - A required check `gerrit/submittable` reports Gerrit's submit
    requirements, so GitHub's merge box reflects whether Gerrit would allow
    the submit.
  - A user who wants to land code from GitHub comments `/submit` (or adds a
    `submit` label). The bridge calls Gerrit
    `POST /changes/{id}/submit` as that user (or on their behalf), which
    submits the whole chain up to that layer.
  - **Implemented** (`internal/reconcile/commands.go`): `/submit` on a
    line of its own in a PR conversation comment submits the change, and
    with it the changes below it, **as the commenter's mapped Gerrit
    account** through `X-Gerrit-RunAs` (the bridge account needs the global
    Run As capability). Gerrit's permissions and submit rules decide;
    a refusal is quoted back on the PR, and unmapped users are told why
    nothing happened. Each command runs once: the reply carries
    `<!-- bridge:cmd/<comment id> -->`, so no state is needed. After the
    merge, Gerrit's events close the PRs, and the bridge deletes its own
    `gerrit/<Change-Id>` branches once no open PR is based on them
    (GitHub-born branches belong to their authors and are kept).
    Verified live on `bridge-scratch`: `/submit` on the top of a two-layer
    GitHub-born stack first quoted Gerrit's refusal (the layer below had a
    newer patch set), then after a restack on GitHub submitted both changes
    as alice; the same on a Gerrit-born chain retargeted the remaining
    layer to `main`.
  - **The native merge button (investigated 2026-10-04).** GitHub merges
    stacks itself: bottom-up, through the merge queue or the asynchronous
    merge API, which is the only merge API that supports stacks
    ([docs](https://docs.github.com/en/pull-requests/get-started/about-stacked-prs)).
    Either way GitHub writes `main`, which Gerrit owns, so the button
    cannot simply be allowed. Options:
    1. **Block it (recommended now).** Protect `main` so only the
       replication account may push; the merge box then says merging is
       blocked, `gerrit/submittable` shows why, and people use `/submit`.
    2. **Treat "add to merge queue" as `/submit`.** With a required merge
       queue, enqueueing fires a `merge_group` `checks_requested` webhook
       and the queue waits for required checks. The bridge could submit
       the PR's change in Gerrit and then fail its check on the merge
       group, so GitHub never merges; replication lands the commits, and
       GitHub marks the PRs merged if their head SHAs reach `main`
       (fast-forward or merge-if-necessary submits only). The queue would
       show the entry as failed, which is confusing, and merge queues need
       an organization-owned repository, so this cannot be tried on
       `bridge-scratch`. Worth an experiment on an org repo.
    3. Letting GitHub merge and replaying into Gerrit would make GitHub the
       source of truth for `main`, contradicting §3.

## 7. Identity

**As built (2026-10-10).** GitHub logins map to the Gerrit account with
the same username, which is how Gerrit's github plugin names the accounts it
creates (AOS uses it); `identity.github_to_gerrit` overrides exceptions.
Everything a mapped GitHub user does reaches Gerrit **as that user** via
Run-As (`X-Gerrit-RunAs`; the bridge needs the global Run As capability):
comments, replies, edits, resolutions (as whoever resolved the thread on
GitHub), uploads (`suexec`), `/submit`, and votes from GitHub reviews
(approve = Code-Review +2, stepped down to +1 if the user may not give +2;
request changes = -1). Gerrit records the bridge as `real_author` and
shows "Performed by … on behalf of …". Because these posts are authored by
the user, the bridge recognizes its own by tag instead (`github-bridge:mirror`
for posts as a user, `autogenerated:github-bridge` for its own), and votes
it cast for a GitHub review are not announced back on the PR. Unmapped
users are posted by the bridge account with their name in the text, and
never vote. A Run-As refusal falls back to the bridge account, minus any
vote: the bridge never votes as itself. Verified live: a `retest` posted on
GitHub by a trusted user started gerrit-buildkite.

**v1: the bot does everything.**
- On GitHub, a GitHub App posts all mirrored content, with an attribution
  header: **Jane Doe** (via Gerrit).
- On Gerrit, a service account posts with an attribution line, or as the
  real user via `X-Gerrit-RunAs` / `on_behalf_of`. Run-As needs no per-user
  secrets, so Gerrit votes from GitHub users can be real votes from day one.
- The identity map (Gerrit account ↔ GitHub login) comes from the
  account's `github:` or `external:` external IDs, which the github plugin
  writes at login, plus overrides in `refs/bridge/config`.

**Later: post to GitHub as the real user, using the github plugin's tokens.**
The plugin (`plugins/github`, master) does persist each user's OAuth token:

- `OAuthWebFilter` encrypts it (AES/ECB with the key file named by
  `github-key.<id>.passwordDevice`) and writes it as an external ID:
  `external:github_oauth:<keyId>:<ciphertext>`, in
  All-Users `refs/meta/external-ids`.
- `IdentifiedUserGitHubLoginProvider` reads it back. No refresh token is
  stored and the tokens don't expire (a classic OAuth App).

So the bridge *can* get a user's token. That needs two things: read access to
external IDs (`GET /accounts/{id}/external.ids` with Access Database, or a
fetch of `refs/meta/external-ids`), and a copy of the plugin's key file.

Caveats:
- The tokens only carry the scopes listed in `github.scopes`. Posting
  reviews needs `repo` or `public_repo`.
- The token is re-issued and re-encrypted at every login.
- The Apache-licensed plugin is deprecated. GerritForge continues it under
  BSL, and storage may differ there.
- The bridge would need the key file from the Gerrit host.

This lives behind an `identity.TokenSource` interface. When there is no
token, the bridge falls back to the bot.

**Watch out:** the github plugin's *pull request import* creates Gerrit
changes from GitHub PRs. That overlaps with §5.2 and would create duplicate
changes, so it must stay off for the bridged repo.

### 7.1 Trust and abuse (reviewed 2026-10-11)

Gerrit stays the gate: everything the bridge does as a user goes through
Run-As or suexec with that user's own permissions, so submit requirements,
label permissions and Verified-from-a-non-uploader still apply. What the
bridge must not do is let someone on GitHub borrow a trusted person's
identity, or make a reviewer's vote count for code they did not see. The
defenses, by threat:

- **Votes on unseen code.** A GitHub review votes only if it was made on
  the commit of Gerrit's current patch set. A review of a commit Gerrit
  does not have (a push not imported yet, or one that cannot be imported)
  is mirrored as text, saying it was not counted.
- **Comments on no commit.** An issue comment, or a review (comment) of a
  commit Gerrit never had, goes on the patch set GitHub had when it was
  written (the newest the bridge had recorded on both sides by then),
  not on whatever is current when the bridge gets to it: otherwise a
  trusted reviewer's `retest`, followed at once by someone's new patch
  set, started CI on code nobody had seen (third audit, confirmed live).
  One written before GitHub had any patch set is posted by the bridge.
- **Edited comments.** Anyone with write access can edit anyone's GitHub
  comment. GraphQL's `lastEditedAt` and `editor` say whether and by whom
  (`updated_at` does not: GitHub moves it without an edit). A comment last
  edited by someone other than its author is posted by the bridge account
  when first mirrored (so a `retest` edited into a maintainer's comment
  does not start CI), and `/submit` does not run from it; later edits are
  always mirrored by the bridge account. The same goes for review bodies,
  and such a review never votes.
- **Edits to the bridge's own GitHub comments.** A Gerrit comment's GitHub
  copy can be edited by anyone with write access, which would put words in
  a Gerrit reviewer's mouth or change its `bridge:gerrit/…` marker. Gerrit
  comments only change in Gerrit, so when a copy was last edited by anyone
  but the bridge, the bridge rewrites it (saying so), and its marker is not
  trusted meanwhile.
- **Who uploaded.** CI trusts the uploader, so nobody is made the
  uploader of code they did not write or vouch for, however casually they
  clicked a GitHub button on someone else's PR (audit 2026-10-18). The
  first upload is the PR author's, if GitHub says they pushed its head
  (anyone may open a PR from someone else's branch, and a branch may
  move before the bridge sees the PR; third audit). A later push builds on the patch set
  GitHub last had (not a newer one only Gerrit has, such as someone
  else's that never reached the PR), and is uploaded as its pusher
  only if they uploaded that patch set too, and only as GitHub
  says: the activity log of the PR's branch (`GET /repos/{repo}/activity`,
  the fork's for a fork PR), which outlives the bridge's restarts and
  covers webhooks it missed; or, for a push the log does not list yet, the
  webhook (`synchronize` or `push` senders; the first push of a commit
  wins; reopening is not pushing). A pure rebase of the current patch set
  (GitHub's Rebase or Update branch: `git merge-tree` of it onto the new
  parent gives the pushed tree, without conflicts) is uploaded on behalf
  of its uploader, as Gerrit's own rebase is, but only onto a parent on
  the target branch or one that uploader uploaded themselves (not on
  their behalf): the parent's code is built too (second and third
  audits). A head that is one of the change's own patch sets (the
  bridge's export of an older one, say) is nobody's push: it is recorded
  and never uploaded again (third audit, seen live: a quick second patch
  set was reverted by the bridge's re-upload of the first). Anything else, and anything no webhook
  explains, is uploaded by the bridge, which CI does not trust;
  a trusted reviewer starts CI. The log is only searched back a few
  hundred entries per branch. The commit's committer email is never
  used, since anyone can write any email.
- **Patch sets someone else uploads in Gerrit.** Gerrit lets anyone with
  Add Patch Set (Registered Users, by default) upload a patch set to any
  change, or edit its commit message. A GitHub-born PR's branch is its
  author's, in their fork or the repository, where on GitHub only they
  and maintainers may push. So a Gerrit patch set reaches it only if its
  uploader, and whoever uploaded it on their behalf, is the change's
  owner, the bridge or in CI's trusted group: anyone else's stays in
  Gerrit, with a notice there once per patch set, and the PR keeps its
  code, base, title and description (third audit, confirmed live: it
  had pushed a Gerrit user's code into a fork).
- **PR edits.** A title or description edit makes a new patch set in
  Gerrit, so it is made only as the current patch set's uploader; anyone
  else is told to edit in Gerrit. Draft toggles and close/reopen run as
  whoever did them. Someone without a Gerrit account (or an edit GitHub
  does not attribute) changes nothing, except on their own PR's
  bridge-owned change, which the bridge edits for them.
- **Accounts the bridge never acts as by matching usernames.** Its own,
  and members of `identity.never_groups` (default Service Users and
  Administrators), which the bridge must be able to see (it refuses to
  start otherwise, and any error looking one up excludes the user: Gerrit
  answers 404 for a group the caller may not see, e.g. Administrators
  unless it is visible to all, audit 2026-10-19), whatever GitHub login matches
  them; an administrator's login must be listed in
  `identity.github_to_gerrit`. The bridge's own GitHub login never maps to
  anyone. A login matching several accounts (by case) matches none.
- **Names Gerrit reads as other accounts.** Wherever Gerrit takes an
  account (`X-Gerrit-RunAs`, `suexec --as`, `accounts/…`), it tries
  `self`/`me` and numeric account ids before usernames, so a user named
  `1000000` stands for account 1000000. Such usernames are never matched or
  acted as (audit 2026-10-18); the REST client and suexec refuse them too.
- **Logins that change hands.** GitHub frees a login when its owner
  renames, and anyone may take it. Each Gerrit account is pinned to the
  numeric GitHub id it was first matched to (`refs/bridge/identities`,
  `identities.json`, a commit per pin); the bridge looks up who holds a
  login now (`GET /users/{login}`, cached 2 minutes) and acts as the Gerrit
  account only if the ids match. Before the first pin of a username
  match, the GitHub account must be older than the Gerrit account
  (`created_at` against `registered_on`): a newer one cannot be the account
  Gerrit's was registered with, so it is someone who took the login of an
  account the bridge had not seen yet (`admin`, an LDAP account, someone
  who renamed). A renamed owner keeps their account
  through an `identity.github_to_gerrit` override, since their id is
  unchanged. (Gerrit's github plugin matches logins at sign-in, so the
  same takeover may work there; the pin only protects the bridge.)
- **Branches Gerrit owns.** The importer refuses a PR whose head is the
  default branch or any branch Gerrit has, and the applier refuses to push
  to the target, the default or any branch Gerrit has. Fork heads are keyed
  `owner/repo:branch` and cannot collide with the repository's branches.
- **What gets uploaded.** A GitHub-born PR's squash must sit on a commit
  already on Gerrit's branch (an unrelated history would bring all its
  ancestors along as changes), and PRs of more than 1000 commits are
  refused, as are PRs bringing a file over 25 MB or over 100 MB in all
  (Gerrit keeps every patch set's objects for good; set its
  `receive.maxObjectSizeLimit` too). Everything fetched from GitHub is
  checked by git as it arrives (`fetch.fsckObjects`), so malformed trees
  and `.gitmodules` tricks never reach the mirror or Gerrit; a PR that
  fails is refused on its own (the others still import), and a head
  refused for its objects or size is not fetched again. A stack's upper
  layer is sized without its lower layers. Closed PRs'
  heads are pruned from the mirror, and fetched branches
  (`refs/remotes/github/heads/…`) and fork PR heads
  (`refs/remotes/github/pull/N`) cannot collide. A Change-Id that belongs to another change is refused (§5.2).
  The bridge reads a PR commit's Change-Id only in its canonical form (one
  `Change-Id: I<40 hex>` footer in the last paragraph) and refuses commits
  with any other line that might name a change (other spellings or
  spacing, more Change-Id lines, a `Link: …/id/I…` footer, which Gerrit
  also reads). And it never uploads a PR's own commit: the upload always
  carries the bridge's message with exactly one Change-Id, so Gerrit
  files it under the change the bridge checked (second audit,
  2026-10-19).
- **Bridge markers.** `<!-- bridge:… -->` markers mark state (handled
  commands, comment mappings) only in comments by the bridge's own GitHub
  login that nobody else has edited, and only at the end, where the
  bridge writes them. Handled commands are also kept in state, so an
  edited reply neither runs its command again nor keeps another from
  running (third audit). Text it copies
  from the other side (Gerrit comments, messages, commit messages, Gerrit's
  error text, quoted code) has its markers broken first (§8), so a Gerrit
  reply cannot mark someone's `/submit` handled or a commit message cut its
  PR description short; the PR description is split at the *last* links
  marker. A marker edited into someone else's GitHub comment does not keep
  it out of Gerrit.
- **Repository configuration from Gerrit.** A change that has files
  under `.github/`, `CODEOWNERS` or `docs/CODEOWNERS` that the target
  branch's tip does not have as they are (so an old base that would bring
  back what the branch has changed counts too), or one with any commit
  not on the branch that changes them (a change below it, even one
  changed back above), is not mirrored: no branch or PR for it, nothing
  more pushed to its PR if it has one, and one notice per patch set in
  Gerrit. The push itself, and the swap before `/submit`, also refuse
  such commits (forks excepted: their workflows run with their own
  secrets), whatever the plan says. GitHub runs workflows from any branch
  pushed to the repository, with its secrets (second audit, confirmed
  live), and reads code owners from a PR's base, which for a stacked PR
  is a branch the bridge pushed (third audit, confirmed live: a Gerrit
  user's CODEOWNERS requested reviews). A change that cannot be checked
  (no merge base, say) is withheld too, and a notice Gerrit will not
  take is dropped: neither stops the branch (third audit).
- **CI status.** Comes from Buildkite's API (§6.5), so no comment can fake
  it. Without Buildkite, only messages by `ci.account` (and never ones
  mirrored from GitHub) drive `gerrit/Verified`'s link and text; with
  `ci.account` unset, no message does.
- **The public `/checks` endpoint** reads changes anonymously, so it shows
  nothing anonymous users cannot already see. Answers are cached briefly,
  failures are not (and a client hanging up does not cause one), at most 4
  are computed at once, and anonymous requests cause at most 300 Gerrit
  reads and 60 Buildkite calls a minute, so they cannot exhaust the rate limit the bridge needs
  for `gerrit/Verified`. Its plugin's Rerun posts as the user, so it
  grants nothing.
- **Webhooks.** HMAC-checked; at most 32 read at once and 4 from any one
  address, each body within 10 seconds, so a few slow senders cannot
  refuse GitHub's deliveries; `github.webhook_allowed_networks` can limit
  them to GitHub's hook ranges. Behind a reverse proxy, list it in
  `github.webhook_trusted_proxies`: its `X-Forwarded-For` then names the
  sender (the nearest hop that is not a trusted proxy), so the allowlist
  and the per-address limits see who sent a delivery, not the proxy; and
  have the proxy buffer request bodies. Anything refused heals at the next resync
  (`resync_interval`, default 30m). Payloads are only hints: actors and
  content are re-read from the API.
- **The server.** Listens on `127.0.0.1:8080` by default, plain HTTP: a
  TLS proxy in front should expose only `/webhook` and `/checks/`.
  `/healthz` (rate budget, queue depth) is for probes and monitoring
  inside the deployment, not for the proxy to expose. The bridge's HTTP
  clients never follow redirects (credentials go only where it sent
  them), and it warns at startup about secret files others can read. `gerrit.url` must be https unless it is a loopback
  address, since every request carries the bridge's password. Responses
  from Gerrit and GitHub are read only up to a bound.
- **PRs GitHub will not take.** A description is cut to fit GitHub's
  65,536-character limit (saying the rest is in Gerrit), chain titles in
  the links block are bounded and have Markdown and HTML escaped, and an
  action GitHub still refuses for one change's PR (a 4xx other than auth
  and rate limits) leaves that change out of the rest of the reconcile
  instead of stopping the branch: anyone registered on Gerrit could
  otherwise stop every PR's updates with one long commit message (third
  audit, confirmed live).
- **Edits over and over.** A PR's edits, draft toggles, closes and
  reopens are mirrored to Gerrit at most once per reconcile; newer ones
  wait for the next, and the plan leaves that PR's text, draft and open
  state alone meanwhile, so someone editing their PR every few seconds
  cannot keep the branch from converging (third audit).
- **API budget.** One key (a PR, a change's comments, a branch) runs at
  most every 10 seconds however many events arrive for it, during or
  after a run, from either side (anyone can sign up on both); only a
  merge's `merged/N` key is not held back, since it races replication, and events on PRs
  opened before `import_created_after` cost one lookup, not a reconcile.
- **Submit swaps.** Before `/submit`, a PR's head is swapped for Gerrit's
  commit only if it is the bridge's record of the patch set being
  submitted; a push not imported yet is never overwritten.
- **Comments GitHub will not take.** Gerrit comments and messages are
  cut to fit GitHub's 65,536 characters (defusing makes them longer),
  quoted lines are bounded, and one GitHub refuses anyway is recorded as
  refused and not retried, so a Gerrit user cannot stop a change's sync
  either (third audit).
- **Comments Gerrit will not take.** A GitHub comment over 12 KiB is
  shortened, with a link to the rest (Gerrit's limit is 16 KiB). One Gerrit
  still refuses (a change's comment-count or total-size limit) is recorded
  as refused and not retried, so a stranger's comment cannot stop a
  change's sync, `/submit` and votes (second audit). A review whose text
  Gerrit refuses still casts its vote alone, and is not dismissed on
  GitHub for lacking one; on a closed change, where Gerrit takes no
  votes, its text still goes in; a GitHub edit Gerrit refuses is dropped
  too (third audit).
- **Public text.** Import errors keep their details in the log, not in the
  public status, and so do `/submit` errors that are not Gerrit's own
  refusals. Mirrored Gerrit comments, messages and `gerrit/<Label>`
  statuses show the username beside the (free-form) full name, with
  Markdown in names escaped.
- **Mentions.** Gerrit text posted on GitHub under the bridge's name, and
  GitHub text the bridge posts in Gerrit as itself, has its `@mentions`
  broken (§8), so nobody can make the bridge notify people or teams.
- `/submit` first asks Gerrit whether the user may submit, and only then
  reshapes PRs (§6.7). It submits the patch set its author saw: one
  uploaded after the comment, or a comment more than a day old (the
  bridge was down, say), or one not on GitHub yet (uploaded in Gerrit
  moments before), is refused with a request to post it again, and
  the revision checked is the one submitted (Gerrit refuses if it is no
  longer current).

Known gaps:

- **First sight is trusted, if the GitHub account is old enough.** A
  login that changed hands to an account older than the Gerrit account
  would still pin. To pin an account up front, write its entry in
  `refs/bridge/identities` by hand.
- **Spam from forks.** Every fork PR becomes a Gerrit change, uploaded by
  the bridge if its author has no account, and every event on it costs
  GitHub API calls. Each PR is rate-limited, but not each author: someone
  opening and commenting on many PRs can still use up the bridge's GitHub
  budget (5000 calls an hour for a token) and stall it until the hour
  resets. Block them on GitHub; a GitHub App would have a larger budget.
- **Visibility.** The bridge mirrors whatever its account can see on the
  target branches except private changes; it must not be able to read
  restricted branches or changes.
- **Gerrit-side tags.** A Gerrit user can tag their own message
  `github-bridge:mirror` and so keep it from being mirrored to GitHub.
- **Sign-offs.** `Signed-off-by` lines in a PR description are kept as
  written; AOS's requirement only checks that the words appear.
- A dismissed GitHub review leaves its Gerrit vote.

## 8. Content conversion (`internal/markup`)

- Gerrit renders a subset of Markdown, and GitHub renders GFM, so most
  content passes through unchanged.
- **As built (2026-10-18, revised after the second audit): copied text is
  defused.** Whatever the bridge posts on one side with words from the
  other (`internal/reconcile/text.go`) has every `@` (literal, as an HTML
  character reference, full-width or small), and `<!-- bridge:` markers,
  broken with a zero-width joiner, which renders as nothing, code
  included: an approximation of what GitHub renders as code was fooled
  four ways. Suggestion blocks stay exact, inside a fence longer than any
  run of backticks in them, so nothing in them is ever outside code.
  Names in bold have Markdown and `&` escaped. The bridge's account
  must not notify whoever the author named, and must not carry markers it
  would read back as its own state. Where PR descriptions flow back into
  commit messages, the joiners are taken out again, so round trips are
  exact. Text posted in Gerrit as its own author (Run-As) is left alone;
  if Run-As is refused and the bridge posts it after all, it is defused
  and marked as posted by the bridge, never in the attribution form the
  bridge reads back as its own copy, and so are GitHub edits the bridge
  mirrors. A suggestion's fix is taken from the raw text, so Gerrit's
  "Apply fix" gets the author's exact code (third audit).
  Rewriting mentions to the mapped user on the other side is not done.
  Closing keywords (`Fixes #12`) are left alone: the commit lands on the
  default branch with them anyway.
- GitHub ```` ```suggestion ```` ⇄ Gerrit `fix_suggestions`
  (`replacements` on the same range). Gerrit's "Apply fix" and GitHub's
  "Commit suggestion" both create a new revision, which then flows through
  §5.
- Edits: a GitHub comment edit becomes a Gerrit reply "edited: …" (Gerrit
  published comments are immutable). The edit is detected by `body_hash`.
  GitHub deletion → nothing on the Gerrit side (log it). Reactions are
  ignored.
- Drafts and pending reviews are never mirrored. Only published content is.

## 9. Operations

- One static binary plus config. An HTTP listener receives GitHub webhooks
  (HMAC verified). For Gerrit, an SSH client runs `gerrit stream-events`,
  which the site's other tooling already uses. It reconnects with backoff.
  `stream-events` cannot replay events missed while it was disconnected,
  so every reconnect queues a full resync of open changes. Because
  reconciliation is level-triggered, that is enough.
- `/healthz` and `/metrics` (Prometheus). Metrics: queue depth, reconcile
  latency, API errors, and GitHub rate-limit budget.
- Rate limits: GitHub ETag conditional requests (as built 2026-10-19:
  every GET is revalidated with `If-None-Match`, and GitHub's 304s, which
  do not count against the rate limit, are answered from an in-memory
  cache of up to 4096 responses); GraphQL for bulk reads; a token bucket
  for each installation. Measured on the test instance: a comment sync
  is ~6 REST GETs and ~6 GraphQL queries per PR, so a resync of a few
  hundred open PRs every 30 minutes would spend most of the hourly
  GraphQL budget; GraphQL has no conditional requests. So GraphQL answers
  that REST can vouch for are reused while every page of the REST lists
  behind them comes back 304: who edited comments (the issue and review
  comment lists; an edit changes the comment's body and `updated_at`) and
  review bodies (the review list). Checked live on 2026-10-19: editing an
  issue comment, a review comment or a review body changes its list's
  ETag (and the PR's); resolving or unresolving a thread changes no list
  and only sometimes the PR, so threads are always asked for, except on a
  PR with no review comments, which has none. A steady PR then costs one
  GraphQL query per sync if it has review comments, else none.
- With `-v`, every Gerrit and GitHub request and git command is logged
  with its duration, and reconciles that wait or take more than 3s are
  always logged.
- `bridge resync <change>` / `bridge resync --all` admin commands, and a dry-run
  mode that only logs the actions it would take. The dry-run mode is essential
  for bringing the bridge up against a real repo.

### 9.1 Deploying on the internet (audit 2026-10-18)

What the site must have before the bridge faces strangers; the test
instance (`internal/testgerrit`) is set up this way.

- **The bridge's Gerrit account:** global Run As and Stream Events through
  its own group. Not in Administrators, Service Users (which vote
  Verified on AOS), the CI trusted group, or any group with label, Submit,
  View Private Changes or Access Database rights. On the bridged project:
  Read on its branches, Push and Create on `refs/for/refs/heads/*`, Add
  Patch Set, Forge Author, and `refs/bridge/*` with exclusive Read plus
  Push and Create. Whatever the bridge's account may do, a GitHub user
  without a Gerrit account effectively may too.
- **Add Patch Set and Forge Author** are Registered Users' by default:
  anyone signed in may upload a patch set to anyone's change, with any
  author. The bridge keeps such patch sets off GitHub-born PRs (§7.1),
  but a Gerrit-born change's PR shows them, and GitHub credits a forged
  author's commits to whoever owns that email on GitHub. Consider
  granting both to maintainers (and the bridge) only: owners may always
  add patch sets to their own changes (third audit).
- **GitHub Actions.** The bridge pushes every open change, from anyone
  registered on Gerrit, to a branch of the repository and opens a PR from
  it, so any workflow that runs on pushes to non-default branches or on
  pull requests runs their code with the repository's (and
  organisation's) secrets and a token that may write. Turn Actions off on
  the bridged repository, or keep every workflow that has secrets or a
  write token to the default branch. The bridge withholds changes that
  touch `.github/` (§7.1) (they would bring their own workflows), and its token
  should lack the `workflow` scope (a fine-grained token or App without
  Workflows), so GitHub refuses such a push even if the bridge tried
  (second audit).
- **Gerrit's github plugin**: its pull-request import stays off for the
  bridged repository (§7), and its webhook (`/plugins/github-plugin/webhook`)
  has a secret (`github.webhookSecret`); without one it accepts unsigned
  deliveries (seen on the test site).
- **The peer key** (`gerrit.ssh.peer_key_file`) can run any SSH command as
  any account, administrators included: a credential for the whole site.
  Keep it only on the bridge's host, mode 600, and rotate it if in doubt.
- **Submit for a submitter group**, not Registered Users (AOS already
  does). Submit also lets one rebase anyone's change on behalf of its
  uploader: with it, anyone could put their code under a trusted
  uploader's name. CI (gerrit-buildkite) must also check a patch set's
  *real* uploader, which Gerrit records for such rebases, not only the
  one it names (second audit, confirmed live).
- **Administrators' `abandon` and `toggleWipState`** on the bridged
  project's `refs/heads/*` (Administrate Server is not applied through
  Run-As), not on All-Projects.
- **`receive.maxObjectSizeLimit`** (say 25m) in gerrit.config.
- **Administrators' GitHub logins** in `identity.github_to_gerrit`: they
  are not matched by username (`identity.never_groups`). Every group
  listed there must be visible to the bridge account (Administrators:
  "Make group visible to all registered users"); the bridge refuses to
  start otherwise.
- **The bridge behind a TLS proxy** that forwards only `/webhook` and
  `/checks/`, buffers request bodies, and is listed in
  `github.webhook_trusted_proxies`; `listen` stays on loopback.
  `gerrit.url` is https.
- **A dedicated GitHub bot account** for the token, not a person's: the
  bridge's own login never maps to a Gerrit account, and a person's
  comments under it would never be mirrored.
- **GitHub's ruleset** lets only replication's deploy key update the
  default branch (§6.7), so nobody merges on GitHub behind Gerrit's back.
- `resync_interval` on (default 30m), and `import_created_after` set when
  the bridge is first turned on.

## 10. Testing

- `internal/reconcile` is tested against in-memory fakes of the Gerrit and
  GitHub interfaces (table tests over state pairs).
- `internal/anchor` has golden tests covering in-hunk lines, out-of-hunk
  lines, PARENT side, deleted lines, ranges and renames.
- Integration: `internal/testgerrit` runs a real Gerrit 3.14.4 from the
  release war, with a pinned JDK, both fetched by Bazel (no Docker). It
  listens on 127.0.0.1 only and uses dev-mode auth. `SetupBridge` creates
  the bridge account, a trusted CI group, users, the project, the
  `refs/bridge/*` ACLs and the Verified label. `//integration` tests use
  it (tagged `integration`, ~15 s); `bazel run //cmd/testgerrit` keeps
  one running in `~/tmp/gerrit-test` and writes a ready-to-use bridge
  config. GitHub: a scratch repo (behind a flag) or a recorded-HTTP fake.

## 11. Milestones

1. **Skeleton:** config, refs/bridge state, queue, Gerrit and GitHub clients, webhook
   ingest, dry-run reconciler. *Done.* GitHub auth is a personal token
   (`token_file` or `token_command: [gh, auth, token]`) behind a
   `TokenSource` interface; a GitHub App source comes once the bridge works.
2. **Gerrit → GitHub code:** branches, PRs, stacks, and lifecycle (read-only
   mirror). Useful on its own. *Done* (`internal/reconcile/plan.go` and
   `code.go`); verified live against `bridge-scratch` (§5.4). Pushes to
   `gerrit/*` branches on GitHub are overwritten until milestone 5, so
   protect them (or tell people not to push there) in the meantime.
3. **Comments both ways:** threads, anchoring, resolution, batching. *Done*
   (`internal/reconcile/comments.go`), verified live on PR #8 / change 4:
   a Gerrit reply became one GitHub review (line and multi-line range
   comments) plus an issue comment for the patch-set-level text; a GitHub
   reply landed in the right Gerrit thread, a GitHub review became one
   `SetReview`, and resolving or "Done" on either side resolved the other.
   How it is built:
   - Comment sync is keyed per change (`comment-added`, `patchset-created`,
     and review/comment webhooks) and shares the branch lock with the
     code mirror.
   - Mirrors are recognisable without state: GitHub bodies carry
     `<!-- bridge:gerrit/<change>/<id> -->`, bridge notices carry
     `<!-- bridge:notice -->`, and Gerrit mirrors are authored by the
     bridge account and link the GitHub original (`#discussion_r…`,
     `#issuecomment-…`, `#pullrequestreview-…`). Lost `refs/bridge` state
     is rebuilt from these without duplicates.
   - Resolution: `threads.json` stores the state both sides last agreed
     on; whichever side differs from it changed (Gerrit wins if both did).
     A GitHub resolve becomes a bridge reply "Resolved on GitHub." with
     `unresolved=false`.
   - Added later (2026-10-04), verified live on PR #9 / change 5:
     - **Suggestions ⇄ fixes** (§8). A Gerrit fix with one replacement in
       the commented file becomes a GitHub ```` ```suggestion ```` on the
       whole lines it touches (partial-line edits are expanded using the
       file). A GitHub suggestion becomes a Gerrit `fix_suggestion`
       ("Apply fix"). Gerrit 3.14 only accepts fix suggestions on human
       comments with `experiments.enabled =
       GerritBackendFeature__allow_fix_suggestions_in_comments`; without
       it the post fails with a 500, and the bridge re-posts the comment
       with the suggestion kept as text.
     - **Edits.** A GitHub edit becomes a reply "edited their comment" in
       the Gerrit thread (published Gerrit comments are immutable). The
       reply links the PR, not the comment, so it is never mistaken for a
       new comment. A Gerrit comment whose text changes (an admin deleting
       it) updates the bridge's GitHub copy. `body_hash` holds the source
       text's hash as last mirrored.
     - **Porting** (`internal/anchor`, an LCS line diff). A Gerrit comment
       GitHub refuses on its own patch set's commit is moved to the same
       unchanged lines of the current patch set ("Ported from patch set
       N"). A GitHub comment on a commit Gerrit never saw (an intermediate
       commit of a layer) is ported into the first patch set whose GitHub
       head contains it (compare API + file contents). Only when the lines
       changed does a comment fall back to file level.
     - Comments are posted against the *GitHub* commit of a patch set,
       which differs from Gerrit's for GitHub-born and re-parented
       revisions.
   - Not yet: GitHub review states other than as text (votes are
     milestone 4).
4. **Votes and CI:** labels ⇄ reviews and checks; identity mapping.
   *Done for Gerrit → GitHub, on a personal token* (decision 2026-10-04:
   no GitHub App yet). Consequences of the token:
   - Check runs need an App, so the bridge sets **commit statuses**
     `gerrit/<Label>` and `gerrit/submittable` on the PR head instead
     (`internal/reconcile/checks.go`). Same states and links as §6.5/§6.7:
     CI labels link to the build from gerrit-buildkite's "Build
     Started/Succeeded/Failed: <url>" messages (`ci.message_pattern`),
     an untrusted uploader gets "CI not started: uploader is not in
     Verified Users…", and `gerrit/submittable` lists the unsatisfied
     submit requirements. Branch protection can require these contexts.
   - GitHub won't let the token's user approve PRs it authored, so
     Gerrit votes are not GitHub reviews. They show in the
     `gerrit/<label>` statuses ("+2 Alice"), and on the mirrored Gerrit
     message they came with ("**Alice** on patch set 3, **Code-Review
     +2**: LGTM"). A vote with no message only updates the status. (Until
     2026-10-17 a separate "Votes in Gerrit" PR comment listed changes.)
   - GitHub approvals stay text in Gerrit ("Approved on GitHub." in the
     mirrored review body). The bridge does not vote on anyone's behalf
     until identity mapping (§7) exists.
   Verified live on PR #8 / change 4 (build started → running with the
   build link; CR+2 and Verified+1 → success; resolving the last thread on
   GitHub → `gerrit/submittable` success) and PR #14 (untrusted uploader).
   Note: replies posted through REST with only a `message` (bots, CLI) are
   change messages, not comments, so they are not mirrored; the web UI's
   reply text is a `/PATCHSET_LEVEL` comment and is.
5. **GitHub → Gerrit code:** deterministic squash and stack import to `refs/for`.
   *Done* (`internal/reconcile/importer.go`, `push.go`), verified live with
   PRs #15/#16 (stack) on `bridge-scratch`:
   - A two-commit layer became one change and the layer above it a second
     one parented on it, both owned and uploaded by the mapped user
     (`identity.github_to_gerrit`, pushed with `suexec`); author kept,
     committer the uploader.
   - Pushing layer 1 without restacking imported patch set 2; layer 2 went
     to a frozen base and the stack split. Restacking on GitHub imported
     layer 2 again and rejoined the stack.
   - A Gerrit edit of a GitHub-born change was appended to the user's
     branch as "Patch set N from Gerrit" (`git pull --ff-only` works); a
     rebase in Gerrit force-pushes instead.
   - Both sides moving: the GitHub push became a patch set, then Gerrit's
     version was re-uploaded on top and appended to GitHub, with a notice on
     both sides.
   Learned while building it:
   - **Same trees, different commits, everywhere.** Any change whose
     parent is GitHub-born must sit on the parent's *GitHub* commit on
     GitHub, or GitHub diffs it against the merge base: a Gerrit-born
     change on a GitHub-born one is re-parented (same tree, message and
     dates), and frozen bases point at the GitHub commit. Statuses go on
     the GitHub commit, and only once it exists.
   - The bridge never overwrites a user's branch at a commit it has not
     imported (e.g. one that needs a restack).
   - A PR the bridge moved to a frozen base is placed, for import, under
     the change its Gerrit parent belongs to.
   - `github.import_created_after` keeps the bridge from importing PRs
     that predate it. (Without it, the first live run imported the old
     experiment PRs #1/#2/#4; their bodies were restored by hand, but the
     pending `gerrit/*` statuses on their commits cannot be deleted.)
   - `resync_interval` re-queues everything periodically, for webhooks that
     cannot reach the bridge.
   Later patch sets are uploaded as whoever pushed them: the `sender` of
   the `pull_request` (opened/synchronize/reopened) or `push` webhook for
   that head commit, else the GitHub user of the head commit's committer
   (its author for web edits by `web-flow`). The first upload stays the PR
   author's, since it decides the owner.
   Added later (2026-10-04), verified live with PRs #22/#23:
   - **`gerrit/sync`** status on every GitHub-born PR head: success ("In
     sync with Gerrit change N, patch set P", noting when the bridge owns
     the change), pending ("Restack needed…", "Waiting for PR #X to
     import", "No commits of its own yet"), failure (a Change-Id that
     belongs to another change) or error. Set only when it changes, and
     import failures are logged only then.
   - **Draft toggles** without a push set Gerrit's WIP flag through
     `/wip` and `/ready`, as the change owner (Run-As), since a re-upload
     of the same commit is "no new changes". The PR text hash no longer
     includes the draft state, so a toggle never makes a patch set.
   - **Commit-message edits in Gerrit** become the PR's title and body
     (bridge trailers removed, links block kept). The new PR text is then
     recorded as already imported, so it does not come back as a patch
     set; a PR edit whose import is still pending is never marked
     imported.
   - **Rule 1 for GitHub-born changes**: a patch set with the same tree
     and parent tree as the GitHub head (a message-only edit) is recorded
     as that head; nothing is pushed.
   - **Signed-off-by.** Projects such as AOS require a `Signed-off-by`
     footer (a `message:"Signed-off-by"` submit requirement). The squash
     keeps every distinct sign-off from the layer's commits, oldest first,
     plus any in the PR description, before the bridge's own trailers;
     `gerrit/sync` notes when there is none. Sign-offs are trailers, so
     they are not copied into the PR text when a Gerrit message edit is
     mirrored back.
   Not yet: non-linear GitHub-born trees.
6. **Submit from GitHub:** `/submit`, plus a merge-queue investigation.
   *Done* (§6.7). Fixed on the way: GitHub refuses to retarget a stacked
   PR, so a stack whose members need a new base is now dissolved first;
   and branches of merged changes are cleaned up even when the merge was
   seen in an earlier reconcile.

## 12. Decisions

- Gerrit owns protected branches. Submit only happens in Gerrit.
- Identity: v1 uses the bot plus Gerrit Run-As. Real GitHub authorship
  through github-plugin tokens comes later (§7).
- Multi-commit GitHub layers are squashed on import, deterministically,
  with matching trees.
- One repo pair. State lives in `refs/bridge/*` in the Gerrit repo, with no
  separate database.
- Gerrit-born branches are named `gerrit/<Change-Id>`. Frozen bases are named
  `gerrit-base/<Change-Id>`.
- CI runs only in Gerrit. GitHub shows the mirrored `gerrit/*` check runs.
- Changes are owned by the real user (SSH `suexec` with a peer key). An
  existing Change-Id on GitHub commits is reused.
- Both sides always link to each other (§6.0).
- Gerrit events come from SSH `stream-events`, plus a resync after every
  reconnect. It is modelled on gerrit-buildkite, with the fixes in §4.2.
- CI trust is gerrit-buildkite's `Verified Users` check on the uploader.
  The bridge keeps it intact by uploading as the real user, and the bridge
  account stays out of the group.
- GitHub approvals and required reviews are not a gate. `gerrit/submittable`
  is the only one.
- Target Gerrit 3.14.4. Build with Bazel and rules_go.

## 13. Open questions

1. ~~Verify the SSH `suexec` peer-key flow for `git-receive-pack` on
   3.14.4.~~ Verified; see §5.2 step 2.
2. ~~Can the native stack merge button be routed to a Gerrit submit?~~
   Not directly; see §6.7 for the options. Option 2 (merge queue entry as
   `/submit`) needs an experiment on an organization-owned repo.
