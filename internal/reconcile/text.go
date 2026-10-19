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

var markerStart = regexp.MustCompile(`(?i)(<!--\s*/?bridge)(:)`)

// atSign matches an @ however it can be written so that GitHub (or Gerrit)
// reads it as one: literally, as an HTML character reference (decoded
// before mentions are found; with or without the semicolon, which HTML
// tolerates), or as a full-width or small at sign.
var atSign = regexp.MustCompile(`(?i)@|&#0*64;?|&#x0*40;?|&commat;|\x{FF20}|\x{FE6B}`)

// noMarkers breaks bridge markers in s, code included: the bridge's
// marker regexps do not know about code.
func noMarkers(s string) string { return markerStart.ReplaceAllString(s, "${1}"+zwj+"${2}") }

// noMentions breaks every @ in s, wherever it is: no mention survives a
// joiner after its @. Code included: telling code from text the way
// GitHub's Markdown renderer does is not something to approximate (the
// second audit found four ways past an approximation), and a joiner in
// code only shows when copied. Suggestion blocks, which must be exact,
// are kept apart by the caller (suggestionFence).
func noMentions(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range atSign.FindAllStringIndex(s, -1) {
		if !isAt(s, m) {
			continue
		}
		b.WriteString(s[last:m[1]] + zwj)
		last = m[1]
	}
	return b.String() + s[last:]
}

// isAt reports whether atSign's match m in s is an @: a character
// reference without its semicolon that runs on into more digits ("&#640;")
// is another character.
func isAt(s string, m []int) bool {
	ref := s[m[0]:m[1]]
	if !strings.HasPrefix(ref, "&#") || strings.HasSuffix(ref, ";") || m[1] == len(s) {
		return true
	}
	next := s[m[1]]
	if ref[2] == 'x' || ref[2] == 'X' {
		return !strings.ContainsRune("0123456789abcdefABCDEF", rune(next))
	}
	return next < '0' || next > '9'
}

// forGitHub defuses text for the bridge to post on GitHub.
func forGitHub(s string) string { return noMentions(noMarkers(s)) }

// forGerrit defuses text the bridge posts in Gerrit as itself (Gerrit
// notifies @mentioned users too). Markers mean nothing in Gerrit.
func forGerrit(s string) string { return noMentions(s) }

var (
	brokenMarker = regexp.MustCompile(`(?i)(<!--\s*/?bridge)` + zwj + `:`)
	brokenAt     = regexp.MustCompile(`(?i)(@|&#0*64;?|&#x0*40;?|&commat;|\x{FF20}|\x{FE6B})` + zwj)
)

// unGitHub removes the joiners forGitHub added.
func unGitHub(s string) string {
	return brokenMarker.ReplaceAllString(brokenAt.ReplaceAllString(s, "${1}"), "${1}:")
}

// inlineName is a free-form name (a Gerrit full name) safe to put in bold
// Markdown: nothing in it can close the bold, make a link or mention.
func inlineName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, `[`, `\[`, `]`, `\]`, `(`, `\(`, `)`, `\)`,
		`&`, `&amp;`, `<`, `&lt;`, `>`, `&gt;`, "`", "\\`", `~`, `\~`, `#`, `\#`, `|`, `\|`).Replace(s)
	return forGitHub(s)
}

// whoMD is who(a) for Markdown: its parts made inline-safe.
func whoMD(a gerrit.AccountInfo) string {
	a.Name, a.Username = inlineName(a.Name), inlineName(a.Username)
	return who(a)
}

// codeSpan is s (a file path, say) as inline code it cannot break out
// of, on one line.
func codeSpan(s string) string {
	s = shorten(strings.Join(strings.Fields(s), " "), 300)
	ticks := strings.Repeat("`", longestBacktickRun(s)+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		pad = " "
	}
	return ticks + pad + s + pad + ticks
}

// longestBacktickRun is the length of the longest run of backticks in s.
func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// suggestionFence is a code fence for text that text itself cannot close:
// longer than any run of backticks in it.
func suggestionFence(text string) string {
	return strings.Repeat("`", max(3, longestBacktickRun(text)+1))
}
