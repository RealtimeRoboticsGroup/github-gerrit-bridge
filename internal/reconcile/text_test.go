package reconcile

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

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
	// References to other characters are left alone; ones that end
	// where the text does are @s.
	for in, want := range map[string]string{"&#640; &#x40a; &#X40F": "&#640; &#x40a; &#X40F", "x&#64": "x&#64" + zwj, "&#x40g": "&#x40" + zwj + "g"} {
		if got := forGitHub(in); got != want {
			t.Errorf("forGitHub(%q) = %q, want %q", in, got, want)
		}
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

// GitHub refuses a description over 65,536 characters, which would stop
// the PR (and once, its branch): a long commit message is cut to fit,
// whole characters only, and the block always stays. Chain titles are
// bounded and cannot carry HTML or links.
func TestPRBodyFitsGitHub(t *testing.T) {
	long := strings.Repeat("é@x ", 30000)
	msg := "Subject\n\n" + long + "\n\nChange-Id: " + cid(3) + "\n"
	chain := []chainEntry{{Change: 2, PatchSet: 1, URL: "u2", Subject: `</a><a href="https://evil">Approved</a>` + strings.Repeat("x", 500)},
		{Change: 3, PatchSet: 1, URL: "u3", This: true}}
	block := linksBlock("u3", 3, 1, chain, "")
	body, _ := composeBody(msg, block)
	if len(body) > maxPRBody || !utf8.ValidString(body) || !strings.HasSuffix(body, block) || !strings.Contains(body, tooLongForGitHub) {
		t.Errorf("body: %d bytes, valid %v, block kept %v", len(body), utf8.ValidString(body), strings.HasSuffix(body, block))
	}
	if strings.Contains(block, "<a href") || strings.Contains(block, strings.Repeat("x", maxLinkTitle+1)) {
		t.Errorf("block = %q", block)
	}
	if short, _ := composeBody("Subject\n\nShort.\n", block); short != "Short.\n\n"+block {
		t.Errorf("short body changed: %q", short)
	}
}

func TestWhoNamesTheUsername(t *testing.T) {
	if got := who(gerrit.AccountInfo{Name: "Austin Schuh", Username: "mallory"}); got != "Austin Schuh (mallory)" {
		t.Errorf("who = %q", got)
	}
	// A name padded to push the username out of a cut status line.
	long := "Austin" + strings.Repeat(" \t\u00a0", 100) + "Schuh" + strings.Repeat("é", 200)
	d := truncateDesc("+2 " + who(gerrit.AccountInfo{Name: long, Username: "mallory"}))
	if !strings.Contains(d, "(mallory)") || !utf8.ValidString(d) || strings.ContainsAny(d, "\t\u00a0") {
		t.Errorf("description = %q", d)
	}
	if d := truncateDesc(strings.Repeat("é", 100)); !utf8.ValidString(d) {
		t.Errorf("cut inside a character: %q", d)
	}
}

// File paths and lines the bridge quotes are code: nothing in them can
// end the code and render as Markdown or HTML.
func TestQuotedCodeStaysCode(t *testing.T) {
	for in, want := range map[string]string{"a.go": "`a.go`", "a`b.go": "``a`b.go``", "`x": "`` `x ``", "a\n**b**": "`a **b**`"} {
		if got := codeSpan(in); got != want {
			t.Errorf("codeSpan(%q) = %q, want %q", in, got, want)
		}
	}
}
