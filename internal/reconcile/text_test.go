package reconcile

import (
	"regexp"
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
)

// Every @ is broken, however it is written and wherever it is: an
// approximation of GitHub's Markdown (code spans, fences) can be fooled.
func TestForGitHubBreaksEveryAt(t *testing.T) {
	in := "Thanks @alice and @org/team, \\@bob /@carol, mail bob@example.com\n" +
		"a lone ` backtick, then @dave\n``code with ` inside`` then @erin\n```x``` @frank\n" +
		"```\n@gina\n~~~\n@hank\n```\n" +
		"&#64;ivan &#064;jane &#x40;kim &#X0040;lee &commat;mo &#64nora &#64;&#117;ser \uff20pat \ufe6bquinn"
	got := forGitHub(in)
	// Every form, listed here independently of atSign.
	every := regexp.MustCompile(`@|&#64;|&#064;|&#x40;|&#X0040;|&commat;|&#64|\x{FF20}|\x{FE6B}`)
	for _, m := range every.FindAllStringIndex(got, -1) {
		if !strings.HasPrefix(got[m[1]:], zwj) {
			t.Errorf("unbroken %q at %d: ...%q", got[m[0]:m[1]], m[0], got[m[0]:min(len(got), m[1]+10)])
		}
	}
	if unGitHub(got) != in {
		t.Errorf("round trip: %q", unGitHub(got))
	}
}

// A suggestion's fence cannot be closed by the suggested text, so the text
// stays code (and exact) whatever it holds.
func TestSuggestionFence(t *testing.T) {
	for _, text := range []string{"x\n", "a ``` b\n", "````\n@everyone\n"} {
		f := suggestionFence(text)
		if strings.Contains(text, f) {
			t.Errorf("fence %q closes inside %q", f, text)
		}
		body := "Try this:\n\n" + f + "suggestion\n" + text + f + "\n\nThanks."
		msg, fixes := fromSuggestion(body, "a.go", 1, 1, 0)
		if len(fixes) != 1 || fixes[0].Replacements[0].Replacement != strings.TrimSuffix(text, "\n") || strings.Contains(msg, "`") {
			t.Errorf("%q: msg %q fixes %+v", text, msg, fixes)
		}
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

func TestInlineNameEscapesReferences(t *testing.T) {
	if got := inlineName("&#64;admin"); strings.Contains(got, "&#64;admin") || !strings.Contains(got, "&amp;") {
		t.Errorf("inlineName = %q", got)
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
