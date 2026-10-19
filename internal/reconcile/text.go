package reconcile

import (
	"regexp"
	"strings"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
)

// Text the bridge copies from one side into what it posts on the other is
// someone else's words under the bridge's name, so it is defused first
// (design §8):
//   - Bridge markers (<!-- bridge:... -->) in it would be read back as the
//     bridge's own state: a Gerrit reply could mark someone's /submit as
//     handled, or a commit message cut its PR's description short.
//   - @mentions in it would have the bridge notify whoever the author
//     named, teams included.
//
// Both are broken with a zero-width joiner, which renders as nothing.
// unGitHub takes the bridge's joiners out again where GitHub text flows
// back (PR descriptions into commit messages), so round trips are exact.
const zwj = "‍"

var (
	markerStart = regexp.MustCompile(`(?i)(<!--\s*/?bridge)(:)`)
	mention     = regexp.MustCompile(`(^|[^A-Za-z0-9_` + "`" + `@/\\])@([A-Za-z0-9])`)
	fence       = regexp.MustCompile("^\\s{0,3}(```|~~~)")
)

// noMarkers breaks bridge markers in s, code included: the bridge's
// marker regexps do not know about code.
func noMarkers(s string) string { return markerStart.ReplaceAllString(s, "${1}"+zwj+"${2}") }

// noMentions breaks @mentions in s outside code, where they would notify
// (in code they are shown as written and notify nobody).
func noMentions(s string) string {
	lines := strings.Split(s, "\n")
	inFence := false
	for i, l := range lines {
		if fence.MatchString(l) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		// Inline code spans: leave the parts between backticks alone.
		parts := strings.Split(l, "`")
		for j := 0; j < len(parts); j += 2 {
			if j == len(parts)-1 && j > 0 && len(parts)%2 == 0 {
				break // an unclosed backtick: GitHub shows it literally
			}
			parts[j] = mention.ReplaceAllString(parts[j], "${1}@"+zwj+"${2}")
		}
		lines[i] = strings.Join(parts, "`")
	}
	return strings.Join(lines, "\n")
}

// forGitHub defuses text for the bridge to post on GitHub.
func forGitHub(s string) string { return noMentions(noMarkers(s)) }

// forGerrit defuses text the bridge posts in Gerrit as itself (Gerrit
// notifies @mentioned users too). Markers mean nothing in Gerrit.
func forGerrit(s string) string { return noMentions(s) }

var brokenMarker = regexp.MustCompile(`(?i)(<!--\s*/?bridge)` + zwj + `:`)

// unGitHub removes the joiners forGitHub added.
func unGitHub(s string) string {
	return brokenMarker.ReplaceAllString(strings.ReplaceAll(s, "@"+zwj, "@"), "${1}:")
}

// inlineName is a free-form name (a Gerrit full name) safe to put in bold
// Markdown: nothing in it can close the bold, make a link or mention.
func inlineName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, `[`, `\[`, `]`, `\]`, `(`, `\(`, `)`, `\)`,
		`<`, `&lt;`, `>`, `&gt;`, "`", "\\`", `~`, `\~`, `#`, `\#`, `|`, `\|`).Replace(s)
	return forGitHub(s)
}

// whoMD is who(a) for Markdown: its parts made inline-safe.
func whoMD(a gerrit.AccountInfo) string {
	a.Name, a.Username = inlineName(a.Name), inlineName(a.Username)
	return who(a)
}
