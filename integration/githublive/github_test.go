// Package githublive checks the GitHub client against the real API. It is
// read-only and runs only when asked:
//
//	bazel test //integration/githublive --test_env=GITHUB_TOKEN=$(gh auth token) \
//	    --test_env=GITHUB_REPO=AustinSchuh/bridge-scratch
package githublive

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

func client(t *testing.T) *github.Client {
	tok, repo := os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_REPO")
	if tok == "" || repo == "" {
		t.Skip("set GITHUB_TOKEN and GITHUB_REPO")
	}
	owner, name, _ := strings.Cut(repo, "/")
	return &github.Client{Owner: owner, Repo: name, Tokens: github.StaticToken(tok)}
}

func TestReadRepoAndStacks(t *testing.T) {
	ctx := context.Background()
	c := client(t)
	repo, err := c.GetRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s default branch %s", repo.FullName, repo.DefaultBranch)

	stacks, err := c.ListStacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stacks {
		t.Logf("stack #%d base=%s open=%v prs=%v", s.Number, s.Base.Ref, s.Open, s.Numbers())
		got, err := c.GetStack(ctx, s.Number)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != s.ID || len(got.PullRequests) != len(s.PullRequests) {
			t.Errorf("GetStack(%d) = %+v, list said %+v", s.Number, got, s)
		}
		// Invariants the bridge relies on (design §5.1).
		prevHead := s.Base.Ref
		for i, sp := range s.PullRequests {
			pr, err := c.GetPull(ctx, sp.Number)
			if err != nil {
				t.Fatal(err)
			}
			if pr.Stack == nil || pr.Stack.Number != s.Number || pr.Stack.Position != i+1 || pr.Stack.Size != len(s.PullRequests) {
				t.Errorf("PR #%d stack = %+v, want #%d position %d", pr.Number, pr.Stack, s.Number, i+1)
			}
			if pr.Base.Ref != prevHead {
				t.Errorf("PR #%d base %s, want %s (previous layer's head)", pr.Number, pr.Base.Ref, prevHead)
			}
			if pr.Head.SHA != sp.Head.SHA {
				t.Errorf("PR #%d head %s, stack says %s", pr.Number, pr.Head.SHA, sp.Head.SHA)
			}
			prevHead = pr.Head.Ref
		}
	}
	prs, err := c.ListPulls(ctx, "all")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d pull requests; rate limit %+v", len(prs), c.RateLimit())
	if c.RateLimit().Limit == 0 {
		t.Error("rate limit headers not recorded")
	}
}
