// Package anchor moves comment anchors between two versions of a file
// (design §6.1 step 6), the way Gerrit ports comments to a new patch set:
// a line keeps its comment if it is unchanged between the versions, at its
// new line number.
package anchor

import "strings"

// Lines splits file content into lines (without the newlines).
func Lines(content string) []string {
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// Map maps unchanged lines of old to their line numbers in new (both
// 1-based), using a longest-common-subsequence diff.
type Map struct {
	toNew map[int]int
}

// NewMap diffs old and new.
func NewMap(old, new []string) *Map {
	m := &Map{toNew: map[int]int{}}
	// Trim the common prefix and suffix first: the usual case is a small
	// edit in a large file, and this keeps the LCS table small.
	pre := 0
	for pre < len(old) && pre < len(new) && old[pre] == new[pre] {
		m.toNew[pre+1] = pre + 1
		pre++
	}
	suf := 0
	for suf < len(old)-pre && suf < len(new)-pre && old[len(old)-1-suf] == new[len(new)-1-suf] {
		m.toNew[len(old)-suf] = len(new) - suf
		suf++
	}
	a, b := old[pre:len(old)-suf], new[pre:len(new)-suf]
	if len(a) == 0 || len(b) == 0 {
		return m
	}
	if int64(len(a))*int64(len(b)) > 25_000_000 {
		return m // too big to diff here; middle lines don't port
	}
	// lcs[i][j] = LCS length of a[i:], b[j:].
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			m.toNew[pre+i+1] = pre + j + 1
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	return m
}

// Line returns line's number in the new version, if it is unchanged.
func (m *Map) Line(line int) (int, bool) {
	n, ok := m.toNew[line]
	return n, ok
}

// Range ports lines first..last. Every line in it must survive, and stay
// contiguous, for the range to port.
func (m *Map) Range(first, last int) (int, int, bool) {
	nf, ok := m.Line(first)
	if !ok {
		return 0, 0, false
	}
	for l := first + 1; l <= last; l++ {
		n, ok := m.Line(l)
		if !ok || n != nf+(l-first) {
			return 0, 0, false
		}
	}
	return nf, nf + (last - first), true
}
