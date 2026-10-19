package reconcile

import (
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

func TestForGitHubBreaksMentionsOutsideCode(t *testing.T) {
	in := "Thanks @alice and @org/team, mail bob@example.com, see https://x.com/@carol.\n" +
		"`@inline` stays\n```\n@fenced stays\n```\n(@paren)"
	got := forGitHub(in)
	for _, broken := range []string{"@" + zwj + "alice", "@" + zwj + "org/team", "(@" + zwj + "paren)"} {
		if !strings.Contains(got, broken) {
			t.Errorf("missing %q in %q", broken, got)
		}
	}
	for _, kept := range []string{"bob@example.com", "x.com/@carol", "`@inline`", "\n@fenced stays\n"} {
		if !strings.Contains(got, kept) {
			t.Errorf("changed %q: %q", kept, got)
		}
	}
	if unGitHub(got) != in {
		t.Errorf("round trip: %q", unGitHub(got))
	}
}

func TestForGitHubBreaksMarkersEverywhere(t *testing.T) {
	in := "x <!-- bridge:cmd/5 --> y\n```\n<!--bridge:gerrit/1/a -->\n```\n<!-- /Bridge:links -->"
	got := forGitHub(in)
	if isMirror(got) || handledMarker.MatchString(got+"") || strings.Contains(strings.ToLower(got), "bridge:") {
		t.Errorf("markers survive: %q", got)
	}
	if unGitHub(got) != in {
		t.Errorf("round trip: %q", unGitHub(got))
	}
}

func TestInlineName(t *testing.T) {
	got := inlineName("Austin** ([Gerrit](https://evil)) @admin <b>")
	for _, bad := range []string{"**", "](", "@admin", "<b>"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q survives in %q", bad, got)
		}
	}
}

// A commit message with mentions and the bridge's own block marker
// round-trips through the PR description unchanged: nothing looks like a
// GitHub edit, and the description is not cut short.
func TestPRBodyRoundTripsDefusedText(t *testing.T) {
	msg := "Subject\n\nThanks @alice. <!-- bridge:links --> not the block.\n\nChange-Id: " + cid(3) + "\n"
	body, _ := composeBody(msg, linksBlock("https://g/c/p/+/3", 3, 1, nil, ""))
	if strings.Contains(body, "@alice") || strings.Count(body, linksStart) != 1 {
		t.Errorf("body = %q", body)
	}
	pr := &github.PullRequest{Title: "Subject", Body: body}
	if githubText(pr) != gerritText(msg) {
		t.Errorf("round trip:\n%q\n%q", githubText(pr), gerritText(msg))
	}
	// A description that has the marker itself is split at the bridge's
	// block, the last one.
	if user, _ := splitBody("mine " + linksStart + " too\n\n" + linksBlock("u", 3, 1, nil, "")); user != "mine "+linksStart+" too" {
		t.Errorf("split = %q", user)
	}
}

func TestWhoNamesTheUsername(t *testing.T) {
	if got := who(gerrit.AccountInfo{Name: "Austin Schuh", Username: "mallory"}); got != "Austin Schuh (mallory)" {
		t.Errorf("who = %q", got)
	}
}
