package reconcile

import "strings"

// Branch names on GitHub (design §5.1). Changes for the default branch use
// the short form; other target branches include the branch so one Change-Id
// can exist on several branches (cherry-picks).
//
// Frozen bases (design §5.4, rule 2) live under a separate prefix:
// "gerrit/<Change-Id>/base" cannot coexist with the branch
// "gerrit/<Change-Id>", because a ref cannot also be a directory.
const (
	headPrefix   = "gerrit/"
	frozenPrefix = "gerrit-base/"
)

// HeadBranch is the GitHub branch holding change changeID's current patch
// set.
func HeadBranch(target, defaultBranch, changeID string) string {
	if target == defaultBranch {
		return headPrefix + changeID
	}
	return headPrefix + target + "/" + changeID
}

// FrozenBranch is the GitHub branch pinning an outdated parent of
// changeID.
func FrozenBranch(target, defaultBranch, changeID string) string {
	if target == defaultBranch {
		return frozenPrefix + changeID
	}
	return frozenPrefix + target + "/" + changeID
}

// frozenFor reports whether branch is a frozen base for target, and the
// Change-Id it belongs to.
func frozenFor(branch, target, defaultBranch string) (string, bool) {
	rest, ok := strings.CutPrefix(branch, frozenPrefix)
	if !ok {
		return "", false
	}
	if target != defaultBranch {
		if rest, ok = strings.CutPrefix(rest, target+"/"); !ok {
			return "", false
		}
	}
	if !isChangeID(rest) {
		return "", false
	}
	return rest, true
}

func isChangeID(s string) bool {
	if len(s) != 41 || s[0] != 'I' {
		return false
	}
	for _, c := range s[1:] {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
