package state

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
)

// newStores returns n stores with separate mirrors of one origin.
func newStores(t *testing.T, n int) (*gitmirror.Repo, []*Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	origin := &gitmirror.Repo{Dir: filepath.Join(dir, "origin.git"), Remote: "unused"}
	if err := origin.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var stores []*Store
	for i := range n {
		r := &gitmirror.Repo{
			Dir:    filepath.Join(dir, "mirror"+string(rune('a'+i))+".git"),
			Remote: origin.Dir,
			Name:   "Bridge", Email: "bridge@example.com",
		}
		if err := r.Init(ctx); err != nil {
			t.Fatal(err)
		}
		stores = append(stores, &Store{Repo: r})
	}
	return origin, stores
}

func sample() *Change {
	return &Change{
		State: ChangeState{
			ChangeID: "I0123", Branch: "main", Origin: "gerrit",
			PR: 4, HeadBranch: "gerrit/I0123", BaseBranch: "main", Lifecycle: "NEW",
		},
		Revisions: []Revision{{PatchSet: 1, GerritSHA: "aa", GitHubSHA: "aa", Tree: "t1", ParentTree: "p1"}},
		Comments:  []Comment{{GerritID: "c1", GitHubID: 99, Kind: "review_comment", Origin: "github", BodyHash: "h"}},
	}
}

func TestRef(t *testing.T) {
	for n, want := range map[int]string{
		1: "refs/bridge/changes/01/1", 12345: "refs/bridge/changes/45/12345", 100: "refs/bridge/changes/00/100",
	} {
		if got := Ref(n); got != want {
			t.Errorf("Ref(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	origin, s := newStores(t, 1)
	c, err := s[0].Load(ctx, 12345)
	if err != nil || c.Version != "" {
		t.Fatalf("Load of new change = %+v, %v", c, err)
	}
	want := sample()
	if err := s[0].Save(ctx, 12345, want, "Link PR #4"); err != nil {
		t.Fatal(err)
	}
	if want.Version == "" || want.State.Change != 12345 {
		t.Fatalf("Save did not set Version/Change: %+v", want.State)
	}
	// It reached the origin.
	if v, _ := origin.RevParse(ctx, Ref(12345)); v != want.Version {
		t.Errorf("origin at %q, want %q", v, want.Version)
	}
	got, err := s[0].Load(ctx, 12345)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load = %+v\nwant %+v", got, want)
	}
	if s[0].ChangeForPR(4) != 12345 {
		t.Error("PR index not updated by Save")
	}

	// Files are stable, pretty JSON; empty lists are [] not null.
	b, err := origin.Git(ctx, nil, "cat-file", "blob", Ref(12345)+":votes.json")
	if err != nil || string(b) != "[]\n" {
		t.Errorf("votes.json = %q, %v", b, err)
	}
	b, _ = origin.Git(ctx, nil, "cat-file", "blob", Ref(12345)+":state.json")
	if !strings.Contains(string(b), "\n  \"pr\": 4,\n") {
		t.Errorf("state.json not indented:\n%s", b)
	}
}

func TestSaveUnchangedIsNoop(t *testing.T) {
	ctx := context.Background()
	_, s := newStores(t, 1)
	c := sample()
	s[0].Save(ctx, 7, c, "first")
	v := c.Version
	if err := s[0].Save(ctx, 7, c, "again"); err != nil {
		t.Fatal(err)
	}
	if c.Version != v {
		t.Error("unchanged Save made a new commit")
	}
	c.State.Stack = 6
	s[0].Save(ctx, 7, c, "stack")
	out, _ := s[0].Repo.Git(ctx, nil, "log", "--format=%s", Ref(7))
	if string(out) != "stack\nfirst\n" {
		t.Errorf("history = %q", out)
	}
}

func TestConcurrentWritersConflict(t *testing.T) {
	ctx := context.Background()
	_, s := newStores(t, 2)
	a, b := s[0], s[1]

	// Both start from "no state" and race to create it.
	ca, cb := sample(), sample()
	if err := a.Save(ctx, 5, ca, "a"); err != nil {
		t.Fatal(err)
	}
	cb.State.Stack = 99
	if err := b.Save(ctx, 5, cb, "b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second create: err = %v, want ErrConflict", err)
	}
	// b's mirror was refreshed: reload and retry succeeds on top of a.
	cb, err := b.Load(ctx, 5)
	if err != nil || cb.Version != ca.Version {
		t.Fatalf("reload = %+v, %v; want a's version", cb, err)
	}
	cb.State.Stack = 99
	if err := b.Save(ctx, 5, cb, "b"); err != nil {
		t.Fatal(err)
	}

	// Now a is stale: its update must be rejected, not overwrite b.
	ca.State.Lifecycle = "MERGED"
	if err := a.Save(ctx, 5, ca, "a2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: err = %v, want ErrConflict", err)
	}
	got, _ := a.Load(ctx, 5)
	if got.State.Stack != 99 || got.State.Lifecycle != "NEW" {
		t.Errorf("after conflict a sees %+v; want b's write", got.State)
	}
}

func TestFetchBuildsIndex(t *testing.T) {
	ctx := context.Background()
	_, s := newStores(t, 2)
	c := sample()
	c.State.PR = 11
	s[0].Save(ctx, 300, c, "x")
	d := sample()
	d.State.PR = 0
	s[0].Save(ctx, 301, d, "y")

	if s[1].ChangeForPR(11) != 0 {
		t.Fatal("index populated before Fetch")
	}
	if err := s[1].Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s[1].ChangesOnBranch("main"); !reflect.DeepEqual(got, []int{300, 301}) {
		t.Errorf("ChangesOnBranch = %v", got)
	}
	if got := s[1].Branches(); !reflect.DeepEqual(got, []string{"main"}) {
		t.Errorf("Branches = %v", got)
	}
	if s[1].ChangeForPR(11) != 300 {
		t.Errorf("ChangeForPR(11) = %d", s[1].ChangeForPR(11))
	}
	nums, err := s[1].Changes(ctx)
	if err != nil || !reflect.DeepEqual(nums, []int{300, 301}) {
		t.Errorf("Changes = %v, %v", nums, err)
	}

	// Relinking a change to another PR drops the old index entry.
	c.State.PR = 12
	s[0].Save(ctx, 300, c, "relink")
	if s[0].ChangeForPR(11) != 0 || s[0].ChangeForPR(12) != 300 {
		t.Errorf("index after relink: 11->%d 12->%d", s[0].ChangeForPR(11), s[0].ChangeForPR(12))
	}
}

func TestSaveRejectsWrongChange(t *testing.T) {
	_, s := newStores(t, 1)
	c := sample()
	c.State.Change = 1
	if err := s[0].Save(context.Background(), 2, c, "x"); err == nil {
		t.Error("saved change 1's state under change 2")
	}
}

func TestIdentities(t *testing.T) {
	_, s := newStores(t, 2)
	ctx := context.Background()
	ids, err := s[0].LoadIdentities(ctx)
	if err != nil || ids.Version != "" || len(ids.ByUser) != 0 {
		t.Fatalf("empty load = %+v, %v", ids, err)
	}
	ids.ByUser["alice"] = Identity{GitHubID: 42, GitHubLogin: "Alice"}
	if err := s[0].SaveIdentities(ctx, ids, "Pin alice"); err != nil {
		t.Fatal(err)
	}
	// The other bridge instance sees it after a fetch, and a stale write
	// conflicts instead of dropping the pin.
	stale, _ := s[1].LoadIdentities(ctx)
	stale.ByUser["bob"] = Identity{GitHubID: 7}
	if err := s[1].SaveIdentities(ctx, stale, "Pin bob"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if err := s[1].Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s[1].LoadIdentities(ctx)
	if err != nil || got.ByUser["alice"].GitHubID != 42 {
		t.Errorf("after fetch: %+v, %v", got, err)
	}
}

// With the index built, conflicts still refresh what Load sees.
func TestConcurrentWritersConflictIndexed(t *testing.T) {
	ctx := context.Background()
	_, s := newStores(t, 2)
	a, b := s[0], s[1]
	for _, st := range s {
		if err := st.Fetch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ca, cb := sample(), sample()
	if err := a.Save(ctx, 5, ca, "a"); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(ctx, 5, cb, "b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second create: err = %v, want ErrConflict", err)
	}
	if got, err := b.Load(ctx, 5); err != nil || got.Version != ca.Version {
		t.Fatalf("b after conflict = %+v, %v; want a's version", got, err)
	}
	cb, _ = b.Load(ctx, 5)
	cb.State.Stack = 99
	if err := b.Save(ctx, 5, cb, "b"); err != nil {
		t.Fatal(err)
	}
	ca.State.Lifecycle = "MERGED"
	if err := a.Save(ctx, 5, ca, "a2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: err = %v", err)
	}
	if got, _ := a.Load(ctx, 5); got.State.Stack != 99 || got.State.Lifecycle != "NEW" {
		t.Errorf("a sees %+v; want b's write", got.State)
	}
}

type countGit struct{ n *int }

func (c countGit) Enabled(context.Context, slog.Level) bool  { return true }
func (c countGit) Handle(context.Context, slog.Record) error { *c.n++; return nil }
func (c countGit) WithAttrs([]slog.Attr) slog.Handler        { return c }
func (c countGit) WithGroup(string) slog.Handler             { return c }

// Once indexed, loading a change already read runs no git at all (a
// reconcile loads every change the bridge ever tracked on the branch),
// and a save is seen at once.
func TestLoadIsCachedOnceIndexed(t *testing.T) {
	ctx := context.Background()
	_, s := newStores(t, 1)
	st := s[0]
	if err := st.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	c := sample()
	if err := st.Save(ctx, 5, c, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(ctx, 5); err != nil {
		t.Fatal(err)
	}
	calls := 0
	st.Repo.Logger = slog.New(countGit{&calls})
	got, err := st.Load(ctx, 5)
	if err != nil || calls != 0 {
		t.Fatalf("second load: %d git commands, err %v", calls, err)
	}
	got.State.Stack = 7 // callers may change what Load gives them
	if again, _ := st.Load(ctx, 5); again.State.Stack != 0 {
		t.Error("a change to a loaded state leaked into the cache")
	}
	got.State.Stack = 8
	if err := st.Save(ctx, 5, got, "two"); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.Load(ctx, 5); again.State.Stack != 8 || again.Version != got.Version {
		t.Errorf("after save: %+v", again)
	}
	if none, err := st.Load(ctx, 6); err != nil || none.Version != "" {
		t.Errorf("unknown change: %+v %v", none, err)
	}
}

// The cache keeps one version per change, however often it is saved.
func TestFileCacheKeepsOnlyCurrentState(t *testing.T) {
	ctx := context.Background()
	_, s := newStores(t, 1)
	st := s[0]
	if err := st.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	c := sample()
	for i := range 20 {
		c.State.Stack = i + 1
		if err := st.Save(ctx, 5, c, "save"); err != nil {
			t.Fatal(err)
		}
		if got, err := st.Load(ctx, 5); err != nil || got.State.Stack != i+1 {
			t.Fatalf("load %d: %+v %v", i, got.State, err)
		}
	}
	if len(st.files) != 1 {
		t.Errorf("cache holds %d versions, want 1", len(st.files))
	}
}

// A failed refresh after a conflict (Gerrit unreachable, say) must not
// make the change look as if it had no state.
func TestConflictRefreshFailureKeepsState(t *testing.T) {
	ctx := context.Background()
	origin, s := newStores(t, 2)
	a, b := s[0], s[1]
	c := sample()
	if err := a.Save(ctx, 5, c, "one"); err != nil {
		t.Fatal(err)
	}
	for _, st := range s {
		if err := st.Fetch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	stale, _ := b.Load(ctx, 5)
	c.State.Stack = 2
	if err := a.Save(ctx, 5, c, "two"); err != nil {
		t.Fatal(err)
	}
	// b can still push (and be refused) but not fetch.
	for _, kv := range [][]string{{"remote.origin.url", "/nonexistent"}, {"remote.origin.pushurl", origin.Dir}} {
		if _, err := b.Repo.Git(ctx, nil, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	stale.State.Stack = 3
	if err := b.Save(ctx, 5, stale, "three"); err == nil {
		t.Fatal("saved over a's write")
	}
	if got, _ := b.Load(ctx, 5); got.Version == "" {
		t.Error("a failed refresh made the state look empty")
	}
}
