package reconcile

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
)

// gitRepo is a scratch work tree whose objects the mirror can read.
type gitRepo struct {
	t   *testing.T
	dir string
}

func (g *gitRepo) run(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Austin", "GIT_AUTHOR_EMAIL=austin@example.com", "GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=Austin", "GIT_COMMITTER_EMAIL=austin@example.com", "GIT_COMMITTER_DATE=1700000000 +0000")
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g *gitRepo) commit(file, content, msg string) string {
	os.WriteFile(filepath.Join(g.dir, file), []byte(content), 0o644)
	g.run("add", file)
	g.run("commit", "-q", "-m", msg)
	return g.run("rev-parse", "HEAD")
}

func newImportEnv(t *testing.T) (*Importer, *gitRepo) {
	dir := t.TempDir()
	g := &gitRepo{t: t, dir: filepath.Join(dir, "work")}
	os.MkdirAll(g.dir, 0o755)
	g.run("init", "-q", "-b", "main")
	m := &gitmirror.Repo{Dir: filepath.Join(g.dir, ".git")}
	im := &Importer{Mirror: m, Logger: slog.Default(),
		GitHub: &github.Client{Owner: "o", Repo: "r"}}
	return im, g
}

func TestSquashIsDeterministicAndKeepsTree(t *testing.T) {
	im, g := newImportEnv(t)
	base := g.commit("a.txt", "base\n", "base")
	g.run("checkout", "-q", "-b", "feature")
	c1 := g.commit("a.txt", "base\none\n", "one")
	h := g.commit("b.txt", "two\n", "two")
	pr := &github.PullRequest{Title: "Feature", Body: "Does things.\n\n" + linksStart + "\nold block\n" + linksEnd,
		HTMLURL: "https://github.com/o/r/pull/3"}
	who := gerrit.AccountInfo{Name: "Alice", Email: "alice@example.com"}
	ctx := context.Background()
	s1, err := im.squash(ctx, pr, []string{h, c1}, h, base, "I"+strings.Repeat("a", 40), "", who)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := im.squash(ctx, pr, []string{h, c1}, h, base, "I"+strings.Repeat("a", 40), "", who)
	if s1 != s2 {
		t.Errorf("squash not deterministic: %s vs %s", s1, s2)
	}
	if g.run("rev-parse", s1+"^{tree}") != g.run("rev-parse", h+"^{tree}") || g.run("rev-parse", s1+"^") != base {
		t.Error("squash changed the tree or parent")
	}
	msg := g.run("log", "-1", "--format=%B", s1)
	for _, want := range []string{"Feature\n\nDoes things.", "GitHub-PR: https://github.com/o/r/pull/3", "Change-Id: I" + strings.Repeat("a", 40)} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "old block") {
		t.Errorf("bridge links block leaked into the commit message:\n%s", msg)
	}
	if got := g.run("log", "-1", "--format=%ae %ce", s1); got != "austin@example.com alice@example.com" {
		t.Errorf("author/committer = %s", got)
	}
}

// A PR's own commit is never uploaded as it is, even a single commit
// with the right Change-Id on the right parent: Gerrit reads Change-Ids
// in more ways than the bridge checks, so the upload always carries the
// bridge's own message.
func TestSquashNeverUploadsThePRsCommit(t *testing.T) {
	im, g := newImportEnv(t)
	base := g.commit("a.txt", "base\n", "base")
	cid := "I" + strings.Repeat("b", 40)
	h := g.commit("a.txt", "x\n", "Do it\n\nChange-Id: "+cid)
	who := gerrit.AccountInfo{Name: "Austin", Email: "austin@example.com"}
	s, err := im.squash(context.Background(), &github.PullRequest{Title: "Do it", HTMLURL: "https://github.com/o/r/pull/1"}, []string{h}, h, base, cid, "", who)
	if err != nil {
		t.Fatal(err)
	}
	if s == h {
		t.Fatal("uploaded the PR's own commit")
	}
	msg := g.run("log", "-1", "--format=%B", s)
	if id, ok := footerChangeID(msg); !ok || id != cid || !strings.Contains(msg, "GitHub-PR: ") {
		t.Errorf("message %q", msg)
	}
}

func TestRealParentIgnoresStaleMergeBase(t *testing.T) {
	im, g := newImportEnv(t)
	g.commit("a.txt", "base\n", "base")
	g.run("update-ref", "refs/remotes/gerrit/heads/main", "HEAD")
	g.run("checkout", "-q", "-b", "l1")
	l1v1 := g.commit("l1.txt", "1\n", "l1")
	g.run("checkout", "-q", "-b", "l2")
	l2a := g.commit("l2.txt", "a\n", "l2 a")
	l2b := g.commit("l2.txt", "b\n", "l2 b")
	// Layer 1 was imported as Gerrit commit "G1" for GitHub head l1v1.
	own, gp, err := im.realParent(context.Background(), "main", l2b, map[string]string{l1v1: "G1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if gp != "G1" || len(own) != 2 || own[0] != l2b || own[1] != l2a {
		t.Errorf("realParent = %v, %s", own, gp)
	}
	// Unknown parent layer version: refuse rather than fold it in.
	if _, _, err := im.realParent(context.Background(), "main", l2b, map[string]string{"other": "G9"}, ""); err == nil {
		t.Error("expected restack-needed error")
	}
}

func TestSquashKeepsSignOffs(t *testing.T) {
	im, g := newImportEnv(t)
	base := g.commit("a.txt", "base\n", "base")
	c1 := g.commit("a.txt", "1\n", "one\n\nSigned-off-by: Austin <austin@example.com>")
	c2 := g.commit("a.txt", "2\n", "two\n\nSigned-off-by: Bob <bob@example.com>\nSigned-off-by: Austin <austin@example.com>")
	pr := &github.PullRequest{Title: "T", Body: "Body.\n\nSigned-off-by: Carol <carol@example.com>", HTMLURL: "u"}
	s, err := im.squash(context.Background(), pr, []string{c2, c1}, c2, base, "I"+strings.Repeat("c", 40), "",
		gerrit.AccountInfo{Name: "A", Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	msg := g.run("log", "-1", "--format=%B", s)
	want := "T\n\nBody.\n\nSigned-off-by: Austin <austin@example.com>\nSigned-off-by: Bob <bob@example.com>\nSigned-off-by: Carol <carol@example.com>\nGitHub-PR: u\nChange-Id: I" + strings.Repeat("c", 40)
	if msg != want {
		t.Errorf("message:\n%s\nwant:\n%s", msg, want)
	}
	if im.lacksSignOff(context.Background(), s) || !im.lacksSignOff(context.Background(), base) {
		t.Error("lacksSignOff wrong")
	}
}

func TestRealParentMustBeInGerrit(t *testing.T) {
	im, g := newImportEnv(t)
	g.commit("a.txt", "base\n", "base")
	g.run("update-ref", "refs/remotes/gerrit/heads/main", "HEAD")
	// A history unrelated to Gerrit's main: importing it would upload
	// every ancestor.
	g.run("checkout", "-q", "--orphan", "other")
	g.commit("x.txt", "1\n", "unrelated root")
	h := g.commit("x.txt", "2\n", "unrelated tip")
	if _, _, err := im.realParent(context.Background(), "main", h, nil, ""); err == nil {
		t.Error("imported a PR whose history does not start from main")
	}
	g.run("checkout", "-q", "-b", "ok", "refs/remotes/gerrit/heads/main")
	h = g.commit("y.txt", "1\n", "fine")
	if own, p, err := im.realParent(context.Background(), "main", h, nil, ""); err != nil || len(own) != 1 || p != g.run("rev-parse", "refs/remotes/gerrit/heads/main") {
		t.Errorf("realParent = %v, %s, %v", own, p, err)
	}
}

// GitHub's "Update branch" merges the base into the PR: the layer sits on
// what it merged, so its squash diffs against that.
func TestRealParentAfterUpdateBranch(t *testing.T) {
	im, g := newImportEnv(t)
	m0 := g.commit("a.txt", "base\n", "base")
	g.run("checkout", "-q", "-b", "feature")
	g.commit("f.txt", "1\n", "feature")
	g.run("checkout", "-q", "main")
	m1 := g.commit("m.txt", "1\n", "main moves")
	g.run("update-ref", "refs/remotes/gerrit/heads/main", m1)
	g.run("checkout", "-q", "feature")
	g.run("merge", "-q", "--no-edit", "main")
	h := g.run("rev-parse", "HEAD")
	own, p, err := im.realParent(context.Background(), "main", h, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if p != m1 || len(own) != 2 {
		t.Errorf("bottom layer: parent %s (m0 %s, m1 %s), own %v; want m1 and the merge plus the feature commit", p, m0, m1, own)
	}

	// Stacked: layer 2 merges layer 1's new head.
	g.run("checkout", "-q", "-b", "l1", m1)
	l1a := g.commit("l1.txt", "a\n", "l1 a")
	g.run("checkout", "-q", "-b", "l2")
	g.commit("l2.txt", "1\n", "l2")
	g.run("checkout", "-q", "l1")
	l1b := g.commit("l1.txt", "b\n", "l1 b")
	g.run("checkout", "-q", "l2")
	g.run("merge", "-q", "--no-edit", "l1")
	h2 := g.run("rev-parse", "HEAD")
	heads := map[string]string{l1a: "G1a", l1b: "G1b"}
	own, p, err = im.realParent(context.Background(), "main", h2, heads, l1b)
	if err != nil {
		t.Fatal(err)
	}
	if p != "G1b" || len(own) != 2 || own[0] != h2 {
		t.Errorf("stacked: parent %s, own %v; want G1b (layer 1's current head) and the merge plus l2", p, own)
	}
	// Without the merge, the parent is the version it was built on.
	if _, p, _ = im.realParent(context.Background(), "main", h2+"^", heads, l1b); p != "G1a" {
		t.Errorf("unmerged: parent %s, want G1a", p)
	}
}

func TestSquashUsesGerritMessage(t *testing.T) {
	im, g := newImportEnv(t)
	base := g.commit("a.txt", "base\n", "base")
	h := g.commit("a.txt", "x\n", "Rebased on GitHub")
	msg := "Subject\n\nBody.\n\nChange-Id: I" + strings.Repeat("d", 40) + "\nSigned-off-by: Austin <austin@example.com>\n"
	s, err := im.squash(context.Background(), &github.PullRequest{Title: "PR title"}, []string{h}, h, base, "I"+strings.Repeat("d", 40), msg,
		gerrit.AccountInfo{Name: "A", Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.run("log", "-1", "--format=%B", s); got != strings.TrimSpace(msg) {
		t.Errorf("message:\n%s\nwant Gerrit's:\n%s", got, msg)
	}
}

func TestFooterChangeID(t *testing.T) {
	a, b := "I"+strings.Repeat("a", 40), "I"+strings.Repeat("b", 40)
	for _, tc := range []struct {
		msg, id string
		ok      bool
	}{
		{"Subject\n\nBody", "", true},
		{"Subject\n\nChange-Id: " + a + "\n", a, true},
		{"Subject\n\nSigned-off-by: X <x@example.com>\r\nChange-Id: " + a + "\r\n", a, true},
		// Gerrit reads the last paragraph; the bridge must not read a
		// different one, or refuse.
		{"Subject\n\nChange-Id: " + a + "\n\nChange-Id: " + b, "", false},
		{"Subject\n\nChange-Id: " + a + "\n\nSigned-off-by: X <x@example.com>", "", false},
		// Anything Gerrit (JGit) might read differently is refused.
		{"Subject\n\nchange-id: " + b, "", false},
		{"Subject\n\n Change-Id: " + a, "", false},
		{"Subject\n\nChange-Id : " + a, "", false},
		{"Subject\n\nChange-Id: " + a + " trailing", "", false},
		{"Subject\n\nLink: https://gerrit.example.com/id/" + b, "", false},
		{"Subject\n\nChange-Id: " + a + "\nLink: https://g/id/" + b, "", false},
		{"Subject\n\nLink: https://example.com/docs", "", true},
		{"Subject\n\nChange-Id: " + a + "\nChange-Id: " + b, "", false},
		{"Subject\n\nChange-Id: Inot-hex", "", false},
	} {
		id, ok := footerChangeID(tc.msg)
		if id != tc.id || ok != tc.ok {
			t.Errorf("footerChangeID(%q) = %q %v, want %q %v", tc.msg, id, ok, tc.id, tc.ok)
		}
	}
}

func TestNoOpSquashNeedsTheFooterChangeID(t *testing.T) {
	im, g := newImportEnv(t)
	base := g.commit("a.txt", "base\n", "base")
	cid, victim := "I"+strings.Repeat("b", 40), "I"+strings.Repeat("c", 40)
	h := g.commit("a.txt", "x\n", "Do it\n\nChange-Id: "+cid+"\n\nChange-Id: "+victim)
	who := gerrit.AccountInfo{Name: "Austin", Email: "austin@example.com"}
	s, err := im.squash(context.Background(), &github.PullRequest{Title: "Do it"}, []string{h}, h, base, cid, "", who)
	if err != nil {
		t.Fatal(err)
	}
	if s == h {
		t.Fatal("uploaded unchanged, so Gerrit would read the footer's Change-Id")
	}
	if id, ok := footerChangeID(g.run("log", "-1", "--format=%B", s)); !ok || id != cid {
		t.Errorf("squash's Change-Id = %q %v, want %s", id, ok, cid)
	}
}

func TestSignOffsCannotCarryOtherLines(t *testing.T) {
	got := signOffs("Fix\n\nSigned-off-by: X <x@example.com>\rChange-Id: I" + strings.Repeat("c", 40) + "\n")
	if len(got) != 1 || strings.Contains(got[0], "\r") {
		t.Errorf("sign-offs = %q", got)
	}
}
