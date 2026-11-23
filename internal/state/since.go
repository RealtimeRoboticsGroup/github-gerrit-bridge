package state

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ReviewersSinceRef records when the reviewer sync first ran on the
// project: since.txt, the time in RFC 3339. A PR created before it is
// taken as it is the first time the sync sees it; a restart does not
// move it (third review: with the process's start time, a PR opened just
// before a restart, or while the bridge was down, lost its first
// requests).
const ReviewersSinceRef = "refs/bridge/reviewers-since"

// ReviewersSince is when the reviewer sync first ran: what the ref says,
// or now, which is recorded if the ref is absent (unless dryRun).
func (s *Store) ReviewersSince(ctx context.Context, now time.Time, dryRun bool) (time.Time, error) {
	for range 3 {
		ver, err := s.Repo.RevParse(ctx, ReviewersSinceRef+"^{commit}")
		if err != nil {
			return time.Time{}, err
		}
		if ver != "" {
			b, err := s.Repo.Git(ctx, nil, "cat-file", "blob", ver+":since.txt")
			if err != nil {
				return time.Time{}, err
			}
			return time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
		}
		if dryRun {
			return now, nil
		}
		out, err := s.Repo.Git(ctx, []byte(now.UTC().Format(time.RFC3339)+"\n"), "hash-object", "-w", "--stdin")
		if err != nil {
			return time.Time{}, err
		}
		out, err = s.Repo.Git(ctx, []byte("100644 blob "+strings.TrimSpace(string(out))+"\tsince.txt\n"), "mktree")
		if err != nil {
			return time.Time{}, err
		}
		out, err = s.Repo.Git(ctx, nil, "commit-tree", strings.TrimSpace(string(out)), "-m", "The reviewer sync starts")
		if err != nil {
			return time.Time{}, err
		}
		// Another instance may record it first: then theirs.
		if err := s.pushRef(ctx, ReviewersSinceRef, "", strings.TrimSpace(string(out))); !errors.Is(err, ErrConflict) {
			return now.UTC().Truncate(time.Second), err
		}
	}
	return time.Time{}, ErrConflict
}
