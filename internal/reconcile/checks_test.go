package reconcile

import (
	"strings"
	"testing"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/buildkite"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/github"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/state"
)

var (
	crValues = map[string]string{"-2": "", "-1": "", " 0": "", "+1": "", "+2": ""}
	vValues  = map[string]string{"-1": "", " 0": "", "+1": ""}
	alice    = gerrit.AccountInfo{AccountID: 1, Name: "Alice"}
	bob      = gerrit.AccountInfo{AccountID: 2, Name: "Bob"}
	ci       = gerrit.AccountInfo{AccountID: 3, Name: "Buildkite"}
)

func change(cr, v []gerrit.ApprovalInfo) *gerrit.ChangeInfo {
	return &gerrit.ChangeInfo{Labels: map[string]gerrit.LabelInfo{
		"Code-Review": {All: cr, Values: crValues},
		"Verified":    {All: v, Values: vValues},
	}}
}

func byContext(ss []github.Status) map[string]github.Status {
	m := map[string]github.Status{}
	for _, s := range ss {
		m[s.Context] = s
	}
	return m
}

func TestStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ch      *gerrit.ChangeInfo
		run     ciRun
		trusted bool
		cr, v   string // state: description
		vURL    string
	}{
		{"nothing yet, trusted uploader", change(nil, nil), ciRun{}, true,
			"pending: No votes yet", "pending: Waiting for CI", "https://g/c/5"},
		{"untrusted uploader", change(nil, nil), ciRun{}, false,
			"pending: No votes yet", "pending: CI not started: uploader is not in Verified Users. A trusted reviewer can comment retest (here or in Gerrit).", "https://g/c/5"},
		{"build running", change(nil, []gerrit.ApprovalInfo{{AccountInfo: ci, Value: 0}}), ciRun{"Started", "https://bk/1"}, true,
			"pending: No votes yet", "pending: CI running", "https://bk/1"},
		{"approved and verified", change([]gerrit.ApprovalInfo{{AccountInfo: alice, Value: 2}, {AccountInfo: bob, Value: 1}},
			[]gerrit.ApprovalInfo{{AccountInfo: ci, Value: 1}}), ciRun{"Succeeded", "https://bk/2"}, true,
			"success: +2 Alice, +1 Bob", "success: +1 Buildkite", "https://bk/2"},
		{"blocked and failed", change([]gerrit.ApprovalInfo{{AccountInfo: alice, Value: 2}, {AccountInfo: bob, Value: -2}},
			[]gerrit.ApprovalInfo{{AccountInfo: ci, Value: -1}}), ciRun{"Failed", "https://bk/3"}, true,
			"failure: -2 Bob", "failure: -1 Buildkite", "https://bk/3"},
		{"weak votes only", change([]gerrit.ApprovalInfo{{AccountInfo: bob, Value: -1}}, nil), ciRun{"Failed", "https://bk/4"}, true,
			"pending: -1 Bob", "pending: CI failed; waiting for the vote", "https://bk/4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := byContext(desiredStatuses(tc.ch, "https://g/c/5", []string{"Verified"}, tc.run, tc.trusted, "Verified Users"))
			if s := got["gerrit/Code-Review"]; s.State+": "+s.Description != tc.cr || s.TargetURL != "https://g/c/5" {
				t.Errorf("Code-Review = %+v, want %q", s, tc.cr)
			}
			if s := got["gerrit/Verified"]; s.State+": "+s.Description != tc.v || s.TargetURL != tc.vURL {
				t.Errorf("Verified = %+v, want %q at %s", s, tc.v, tc.vURL)
			}
		})
	}
}

func TestSubmittableStatus(t *testing.T) {
	ch := change(nil, nil)
	ch.SubmitRequirements = []gerrit.SubmitRequirementResult{
		{Name: "Code-Review", Status: "UNSATISFIED"}, {Name: "Verified", Status: "SATISFIED"}, {Name: "No-Unresolved-Comments", Status: "UNSATISFIED"},
	}
	s := byContext(desiredStatuses(ch, "u", nil, ciRun{}, true, ""))[submittableStatus]
	if s.State != "pending" || s.Description != "Needs Code-Review, No-Unresolved-Comments" {
		t.Errorf("submittable = %+v", s)
	}
	ch.Submittable = true
	if s := byContext(desiredStatuses(ch, "u", nil, ciRun{}, true, ""))[submittableStatus]; s.State != "success" {
		t.Errorf("submittable = %+v", s)
	}
}

func TestLatestCIRunOnlyCurrentPatchSet(t *testing.T) {
	ci := gerrit.AccountInfo{Username: "buildkite"}
	ch := &gerrit.ChangeInfo{Messages: []gerrit.ChangeMessage{
		{RevisionNumber: 1, Author: ci, Message: "Build Failed: https://bk/1"},
		{RevisionNumber: 2, Author: ci, Message: "Patch Set 2:\n\nBuild Started: https://bk/2"},
		{RevisionNumber: 2, Author: ci, Message: "Patch Set 2: Verified+1\n\nBuild Succeeded: https://bk/2"},
		{RevisionNumber: 2, Message: "Patch Set 2: Code-Review+2"},
		// Anyone can write CI's words, in Gerrit or (mirrored) on GitHub.
		{RevisionNumber: 2, Author: gerrit.AccountInfo{Username: "mallory"}, Message: "Build Failed: https://evil/"},
		{RevisionNumber: 2, Author: gerrit.AccountInfo{Username: "buildkite"}, Tag: mirrorTag, Message: "Build Failed: https://evil/"},
	}}
	if run := latestCIRun(ch, 2, DefaultCIMessage, "buildkite"); run != (ciRun{"Succeeded", "https://bk/2"}) {
		t.Errorf("run = %+v", run)
	}
	if run := latestCIRun(ch, 3, DefaultCIMessage, "buildkite"); run != (ciRun{}) {
		t.Errorf("run on ps 3 = %+v", run)
	}
}

func TestLatestBuild(t *testing.T) {
	builds := []buildkite.Build{
		{Number: 4, State: "failed", WebURL: "u4"},
		{Number: 6, State: "running", WebURL: "u6"},
		{Number: 5, State: "passed", WebURL: "u5"},
	}
	if run := latestBuild(builds); run != (ciRun{"Started", "u6"}) {
		t.Errorf("latestBuild = %+v", run)
	}
	builds[1].State = "canceled"
	if run := latestBuild(builds); run != (ciRun{"Failed", "u6"}) {
		t.Errorf("latestBuild = %+v", run)
	}
	if run := latestBuild(builds[2:]); run != (ciRun{"Succeeded", "u5"}) {
		t.Errorf("latestBuild = %+v", run)
	}
	if run := latestBuild(nil); run != (ciRun{}) {
		t.Errorf("latestBuild(nil) = %+v", run)
	}
}

func TestVotesComment(t *testing.T) {
	ch := change([]gerrit.ApprovalInfo{{AccountInfo: alice, Value: 2}, {AccountInfo: bob, Value: 0}},
		[]gerrit.ApprovalInfo{{AccountInfo: ci, Value: 1}})
	cur := humanVotes(ch, []string{"Verified"})
	if len(cur) != 1 || cur[0] != (state.Vote{Account: 1, Label: "Code-Review", Value: 2}) {
		t.Fatalf("humanVotes = %+v (CI and zero votes must be left out)", cur)
	}
	msg := votesComment([]state.Vote{{Account: 2, Label: "Code-Review", Value: -1}}, cur, ch.Labels, "https://g/c/5")
	for _, want := range []string{"**Alice**: Code-Review +2", "**Bob**: Code-Review vote removed", votesMarker} {
		if !strings.Contains(msg, want) {
			t.Errorf("comment missing %q:\n%s", want, msg)
		}
	}
	if !isMirror(msg) {
		t.Error("votes comment would be mirrored back to Gerrit")
	}
	if votesComment(cur, cur, ch.Labels, "u") != "" {
		t.Error("unchanged votes produced a comment")
	}
}

func TestVotesFromGitHubAreNotAnnouncedBack(t *testing.T) {
	ch := change([]gerrit.ApprovalInfo{{AccountInfo: alice, Value: 2, Tag: mirrorTag}, {AccountInfo: bob, Value: 1}}, nil)
	msg := votesComment(nil, humanVotes(ch, []string{"Verified"}), ch.Labels, "u")
	if strings.Contains(msg, "Alice") || !strings.Contains(msg, "**Bob**: Code-Review +1") {
		t.Errorf("votes comment = %q", msg)
	}
}
