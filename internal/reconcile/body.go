package reconcile

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
)

// The PR body is the commit message (minus its subject, which is the
// title) followed by a block the bridge owns (design §6.0). The block is
// rewritten on every reconcile; the part above it is rewritten only when
// the commit message changes, so edits made on GitHub survive until the
// next commit-message edit in Gerrit.
const (
	linksStart = "<!-- bridge:links -->"
	linksEnd   = "<!-- /bridge:links -->"
)

// messageBody is the commit message without its subject paragraph and
// the trailers the PR has no use for.
func messageBody(message string) string {
	message = strings.ReplaceAll(message, "\r\n", "\n")
	if i := strings.Index(message, "\n\n"); i >= 0 {
		// Change-Id and sign-offs are Gerrit's business, not the PR's.
		return strings.TrimSpace(bridgeTrailers.ReplaceAllString(message[i+2:], ""))
	}
	return ""
}

// splitBody splits a PR body into the part before the links block and
// whether a block was present.
func splitBody(body string) (user string, hasBlock bool) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	// The bridge's block is the last thing in the body.
	i := strings.LastIndex(body, linksStart)
	if i < 0 {
		return strings.TrimSpace(body), false
	}
	return strings.TrimSpace(body[:i]), true
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:8])
}

// chainEntry is one line of the relation chain in the links block.
type chainEntry struct {
	Change   int
	PatchSet int
	URL      string
	PR       int
	Subject  string
	This     bool
}

// gerritLogo is Gerrit's favicon (the same file every Gerrit serves), from
// Gerrit's own review site; GitHub proxies it, so viewers do not load it
// from there. The test instance's Gerrit is not reachable from GitHub.
const gerritLogo = `<img src="https://gerrit-review.googlesource.com/favicon.ico" alt="Gerrit" height="16" align="absmiddle">`

// githubLogo is GitHub's favicon, marking the PR links.
const githubLogo = `<img src="https://github.githubassets.com/favicons/favicon.png" alt="GitHub" height="16" align="absmiddle">`

// linksBlock renders the bridge-owned block.
func linksBlock(changeURL string, number, patchSet int, chain []chainEntry, note string) string {
	var b strings.Builder
	b.WriteString(linksStart + "\n---\n")
	if len(chain) == 0 {
		chain = []chainEntry{{Change: number, PatchSet: patchSet, URL: changeURL, This: true}}
	}
	// The relation chain, top first as Gerrit lists it, even for a single
	// change: its own line is this PR's link to Gerrit.
	b.WriteString("**Relation chain**\n\n")
	var lines []string
	for _, e := range slices.Backward(chain) {
		title := e.Subject
		if title == "" {
			title = fmt.Sprintf("change %d", e.Change)
		}
		line := fmt.Sprintf("[%s, patch set %d](%s/%d)", forGitHub(escapeLinkText(title)), e.PatchSet, e.URL, e.PatchSet)
		switch {
		case e.PR != 0:
			line += fmt.Sprintf(" (%s #%d)", githubLogo, e.PR)
		case !e.This:
			line += " (no PR yet)"
		}
		if e.This {
			line = "**" + line + "** ← this PR"
		}
		lines = append(lines, gerritLogo+" "+line)
	}
	// GitHub keeps line breaks in PR bodies.
	b.WriteString(strings.Join(lines, "\n") + "\n")
	if note != "" {
		b.WriteString("\n" + note + "\n")
	}
	b.WriteString(linksEnd)
	return b.String()
}

// composeBody returns the PR body for a commit message: its description
// (without its subject and the bridge's trailers) and the links block, and
// the description's hash.
func composeBody(message, block string) (body, hash string) {
	want := forGitHub(messageBody(message))
	hash = hashString(want)
	if want == "" {
		return block, hash
	}
	return want + "\n\n" + block, hash
}

// escapeLinkText keeps a commit subject from breaking out of a Markdown
// link's text.
func escapeLinkText(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(s)
}
