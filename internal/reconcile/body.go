package reconcile

import (
	"crypto/sha256"
	"fmt"
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

// messageBody is the commit message without its subject paragraph.
func messageBody(message string) string {
	message = strings.ReplaceAll(message, "\r\n", "\n")
	if i := strings.Index(message, "\n\n"); i >= 0 {
		return strings.TrimSpace(message[i+2:])
	}
	return ""
}

// splitBody splits a PR body into the part before the links block and
// whether a block was present.
func splitBody(body string) (user string, hasBlock bool) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	i := strings.Index(body, linksStart)
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
	Change  int
	URL     string
	PR      int
	Subject string
	This    bool
}

// linksBlock renders the bridge-owned block.
func linksBlock(changeURL string, number, patchSet int, chain []chainEntry, note string) string {
	var b strings.Builder
	b.WriteString(linksStart + "\n---\n")
	fmt.Fprintf(&b, "**Gerrit:** [change %d](%s), patch set %d. Review here or in Gerrit; Gerrit submits.\n", number, changeURL, patchSet)
	if len(chain) > 1 {
		b.WriteString("\n**Relation chain** (bottom first):\n")
		for i, e := range chain {
			pr := "no PR yet"
			if e.PR != 0 {
				pr = fmt.Sprintf("#%d", e.PR)
			}
			line := fmt.Sprintf("[%d](%s) (%s) %s", e.Change, e.URL, pr, e.Subject)
			if e.This {
				line = "**" + line + "** ← this PR"
			}
			fmt.Fprintf(&b, "%d. %s\n", i+1, line)
		}
	}
	if note != "" {
		b.WriteString("\n" + note + "\n")
	}
	b.WriteString(linksEnd)
	return b.String()
}

// composeBody returns the body to write, given the current PR body (empty
// for a new PR), the commit message, the hash of the message part the
// bridge last wrote, and the links block.
func composeBody(current, message, lastHash, block string) (body, hash string) {
	want := messageBody(message)
	hash = hashString(want)
	user, hasBlock := splitBody(current)
	part := want
	if hasBlock && hash == lastHash {
		// The commit message has not changed since the bridge last wrote
		// the body: keep whatever is there now, including GitHub edits.
		part = user
	}
	if part == "" {
		return block, hash
	}
	return part + "\n\n" + block, hash
}
