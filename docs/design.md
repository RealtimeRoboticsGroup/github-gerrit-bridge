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

Events are only *hints*. Every event reduces to "reconcile the stack that
contains change C". The reconciler then:

1. Fetches the current full state of C (and the rest of its chain) from
   **both** systems.
2. Loads the mapping state from `refs/bridge/changes/...`.
3. Works out the actions needed to make the two sides match, and applies them
   idempotently.
4. Records the new mappings in the same transaction it uses to mark the
   actions done.

This survives dropped webhooks, restarts, races and bridge downtime. A
periodic poller re-queues every open change, so missed events are
self-healing. The queue is de-duplicated per stack, and only one worker
handles a given stack at a time, so there are no concurrent edits to one
chain.

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
| Handles events inline on the read goroutine. A failed build trigger retries every 30 s **while blocking the read**. | Gerrit's per-client queue fills, and it sends `dropped-output` and discards events. | The read loop only parses and enqueues a key (project, change) into the de-duplicating work queue. It never blocks on the network. |
| Reconnects right away in a tight loop | If Gerrit is down, the loop spins hot. | Exponential backoff with jitter, capped at 1 min. |
| No catch-up after a disconnect | Events sent while disconnected are lost. | Every (re)connect, and every `dropped-output` event, queues a full resync of open changes. Reconciliation is level-triggered, so that is all the recovery needed. |
| Subscribes to every event type | Volume. | `stream-events -s patchset-created -s comment-added -s change-abandoned -s change-restored -s change-merged -s wip-state-changed -s private-state-changed -s reviewer-added -s reviewer-deleted -s vote-deleted -s hashtags-changed -s topic-changed -s ref-updated -s dropped-output`. |
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
2. Force-push it to `refs/heads/gerrit/<Change-Id>` on GitHub. This branch
   is bridge-owned, and the name stays the same across patch sets. One
   Change-Id can exist on several target branches (for example
   cherry-picks), so changes that target anything other than the default
   branch use `gerrit/<target-branch>/<Change-Id>`.
3. Make sure a PR exists with `head=gerrit/<Change-Id>` and
   `base=gerrit/<parent Change-Id>` (or the target branch for the bottom
   change).
   Title and body come from the commit message. The PR body links back to
   Gerrit.
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
     Committer: the bridge.
   - **No-op squash:** if the layer is exactly one commit, that commit
     already has the chosen Change-Id, and its parent is the Gerrit commit
     chosen above, then `S_k = H_k`. Nothing is rewritten, and the SHAs
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
     `ssh gerrit suexec --as <account> -- git-receive-pack '/<project>'`
     through `GIT_SSH_COMMAND`. **To verify on 3.14.4** (§13).
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
- **Parent changed** (rebased in Gerrit): force-push the Gerrit commit
  itself. This is the same experience as a GitHub-side restack, and
  `gh stack sync` recovers it.

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
comments, because GitHub re-maps against the PR diff either way. Appending
only protects the GitHub user's local branch.

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

The fix. The bridge creates a branch `gerrit/<C's Change-Id>/base`
pointing **at B1**, and changes PR-C's base to it:

```
Gerrit                         GitHub
  C   parent = B1                PR-C   base: gerrit/C/base (= B1)  ← own stack
  B2  parent = A                 PR-B   base: gerrit/A    ┐ stack
  A   parent = main              PR-A   base: main        ┘
```

- GitHub now diffs PR-C against B1, exactly as Gerrit does.
- The cost: PR-C has to **leave the stack**. GitHub only allows PRs in one
  stack when each PR's base is the head branch of the PR below it, and
  `gerrit/C/base` isn't PR-B's head branch. So the stack becomes [A, B],
  and C, with anything above it, forms its own stack.
- The bridge comments on PR-C: "Based on an outdated version of PR-B.
  Rebase in Gerrit or run `gh stack rebase`."
- When someone rebases C onto B2, from either side, PR-C's base goes back
  to `gerrit/B`, the stack becomes [A, B, C] again, and `gerrit/C/base` is
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
`…/base` branch at its real parent and starts a new stack.

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
    new patch set. With one commit per layer and a Change-Id present, this
    is a no-op squash with identical SHAs.
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
  - PR3 moves to a frozen base `gerrit/<C3>/base` (pointing at C2 patch
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
  `<!-- bridge:links -->` markers. It holds the Gerrit change URL and
  number, the current patch set, and the Gerrit relation chain (each entry
  with its matching PR). The bridge rewrites only this block, so users'
  edits elsewhere in the body are kept.
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
    `retest`. The comment is mirrored with Run-As (§7), so Gerrit sees
    them as the author and gerrit-buildkite triggers. The mirrored body must
    keep `retest` on a line of its own: the attribution header and links go
    on separate lines.
  - The bridge reads the same group (REST
    `GET /groups/{id}/members?recursive`, cached briefly) so GitHub can
    explain what's going on. When the uploader isn't recognized,
    `gerrit/Verified` stays *queued* with the summary "CI not started:
    uploader is not in Verified Users. A trusted reviewer can comment
    `retest`."
  - The group name is a config value (`ci.trusted_group`, default
    `Verified Users`).
- **gerrit-buildkite's messages map onto check runs.**
  - "Build Started: <url>" along with `Verified=0` → `gerrit/Verified`
    *in_progress*, `details_url = <url>`, so the PR links straight to the
    Buildkite build.
  - "Build Succeeded|Failed: <url>" along with `Verified=±1` →
    *completed* (success/failure).
  - The rule matches on the CI account and a message regex in config, so
    other CI systems can be added the same way.

### 6.6 Lifecycle

| Event                     | Mirror                                          |
|---------------------------|-------------------------------------------------|
| WIP on/off                | Draft / ready for review                        |
| Abandon / Restore         | Close / Reopen (stack: `/unstack` that layer)   |
| Private change            | **Never mirrored**                              |
| Reviewer added / attention| Request review (mapped users only)              |
| Hashtags                  | Labels (optional)                               |
| Commit message edit (new PS) | PR title/body updated                        |

### 6.7 Submit and merge

- **Gerrit submit** (single change or the whole chain): Gerrit updates
  `main`, and replication copies it to GitHub. If submit used
  fast-forward/merge, GitHub sees the PR head SHAs in `main` and
  auto-detects the merge. If submit used rebase/cherry-pick, the SHAs differ,
  so the bridge closes each PR with "Merged as \<sha\> in Gerrit change N"
  and deletes the `gerrit/<Change-Id>` branches. GitHub retargets the remaining stack
  layers.
- **GitHub merge button**: branch protection lets only the bridge App push
  to `main`, so a GitHub merge cannot happen directly. Instead:
  - A required check `gerrit/submittable` reports Gerrit's submit
    requirements, so GitHub's merge box reflects whether Gerrit would allow
    the submit.
  - A user who wants to land code from GitHub comments `/submit` (or adds a
    `submit` label). The bridge calls Gerrit
    `POST /changes/{id}/submit` as that user (or on their behalf), which
    submits the whole chain up to that layer.
  - **To investigate:** whether a GitHub App can intercept the native stack
    merge, for example through merge queue integration, so the real merge
    button works. Merge queue with an external "merger" would be ideal.

## 7. Identity

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

## 8. Content conversion (`internal/markup`)

- Gerrit renders a subset of Markdown, and GitHub renders GFM, so most
  content passes through unchanged. Each GitHub `@mention` is rewritten to
  the mapped Gerrit user's name, and the reverse.
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
- Rate limits: GitHub ETag conditional requests; GraphQL for bulk reads; a
  token bucket for each installation.
- `bridge resync <change>` / `bridge resync --all` admin commands, and a dry-run
  mode that only logs the actions it would take. The dry-run mode is essential
  for bringing the bridge up against a real repo.

## 10. Testing

- `internal/reconcile` is tested against in-memory fakes of the Gerrit and
  GitHub interfaces (table tests over state pairs).
- `internal/anchor` has golden tests covering in-hunk lines, out-of-hunk
  lines, PARENT side, deleted lines, ranges and renames.
- Integration: the `gerritcodereview/gerrit` Docker image, plus either a
  scratch GitHub repo (behind a flag) or a recorded-HTTP fake for CI.

## 11. Milestones

1. **Skeleton:** config, refs/bridge state, queue, Gerrit and GitHub clients, webhook
   ingest, dry-run reconciler.
2. **Gerrit → GitHub code:** branches, PRs, stacks, and lifecycle (read-only
   mirror). Useful on its own.
3. **Comments both ways:** threads, anchoring, resolution, batching.
4. **Votes and CI:** labels ⇄ reviews and checks; identity mapping.
5. **GitHub → Gerrit code:** deterministic squash and stack import to `refs/for`.
6. **Submit from GitHub:** `/submit`, plus a merge-queue investigation.

## 12. Decisions

- Gerrit owns protected branches. Submit only happens in Gerrit.
- Identity: v1 uses the bot plus Gerrit Run-As. Real GitHub authorship
  through github-plugin tokens comes later (§7).
- Multi-commit GitHub layers are squashed on import, deterministically,
  with matching trees.
- One repo pair. State lives in `refs/bridge/*` in the Gerrit repo, with no
  separate database.
- Gerrit-born branches are named `gerrit/<Change-Id>`. Frozen bases are named
  `gerrit/<Change-Id>/base`.
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

1. Verify the SSH `suexec` peer-key flow for `git-receive-pack` on 3.14.4,
   so changes are owned by the right user (§5.2 step 2).
2. Can the native stack merge button be routed to a Gerrit submit (through
   merge queue integration)? Needs an experiment.
