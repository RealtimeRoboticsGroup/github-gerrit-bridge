package testgerrit

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/AustinSchuh/github-gerrit-bridge/internal/gerrit"
	"github.com/AustinSchuh/github-gerrit-bridge/internal/gitmirror"
)

// The test site's access rules are AOS's own (realtimeroboticsgroup.org,
// read 2026-10-19), plus exactly what the bridge needs on top: the
// deployment checklist (design §9.1) is the difference.
var (
	//go:embed aos/All-Projects.project.config
	AOSAllProjects string
	//go:embed aos/aos.project.config
	AOSProject string
)

// AOSGroups are the internal groups AOS's access rules name.
var AOSGroups = []string{"Administrators", "Forge Author", "Service Users", "Submitters", "Sudo", "Verified Users"}

// Gerrit 3.14 refuses to store a legacy label function (AOS's still carry
// MaxWithBlock from an older version, which Gerrit applies as a submit
// requirement): stored as NoBlock with that requirement spelled out.
var (
	AllProjectsFor314 = [][]string{
		{"label.Code-Review.function", "NoBlock"},
		{"submit-requirement.Code-Review.description", "Changes must have at least one MAX Code-Review vote and no MIN to be submittable."},
		{"submit-requirement.Code-Review.submittableIf", "label:Code-Review=MAX AND -label:Code-Review=MIN"},
		{"submit-requirement.Code-Review.canOverrideInChildProjects", "true"},
	}
	ProjectFor314 = [][]string{
		{"label.Verified.function", "NoBlock"},
	}
)

// BridgeAllProjects and BridgeProject are what the bridge needs added to
// AOS's All-Projects and project (design §9.1).
var (
	BridgeAllProjects = [][]string{
		// Run As (votes, /submit, mirrored edits, closes) and Stream
		// Events, through its own group: not Service Users, which vote
		// Verified.
		{"--add", "capability.runAs", "group " + BridgeGroup},
		{"--add", "capability.streamEvents", "group " + BridgeGroup},
	}
	BridgeProject = [][]string{
		// Only the bridge can see or write its state (design §4.4).
		{"--add", "access.refs/bridge/*.exclusiveGroupPermissions", "read"},
		{"--add", "access.refs/bridge/*.read", "group " + BridgeGroup},
		{"--add", "access.refs/bridge/*.push", "group " + BridgeGroup},
		{"--add", "access.refs/bridge/*.create", "group " + BridgeGroup},
		// Imports and Gerrit-born exports keep their real authors.
		{"--add", "access.refs/*.forgeAuthor", "group " + BridgeGroup},
		// Administrate Server is not applied through Run-As: closing a
		// PR (abandon) or marking it a draft (WIP) as an administrator
		// needs the explicit grants, on this project only.
		{"--add", "access.refs/heads/*.abandon", "group Administrators"},
		{"--add", "access.refs/heads/*.toggleWipState", "group Administrators"},
	}
)

// ConfigureAllProjects gives All-Projects AOS's access rules, plus the
// bridge's (BridgeAllProjects), creating the groups they name. Unchanged,
// it pushes nothing.
func (s *Server) ConfigureAllProjects(ctx context.Context) error {
	for _, g := range append(AOSGroups, BridgeGroup) {
		if err := s.CreateGroup(ctx, g); err != nil {
			var ge *gerrit.Error
			if !errors.As(err, &ge) || ge.Status != http.StatusConflict {
				return fmt.Errorf("group %s: %w", g, err)
			}
		}
	}
	// A new site's administrators may not push refs/meta/config yet (AOS's
	// may, so the config written below keeps it).
	admins, err := s.groupID(ctx, "Administrators")
	if err != nil {
		return err
	}
	if err := s.Admin.Do(ctx, http.MethodPost, "projects/All-Projects/access", map[string]any{
		"add": map[string]any{"refs/meta/config": map[string]any{"permissions": map[string]any{
			"push": map[string]any{"rules": map[string]any{admins: map[string]any{"action": "ALLOW"}}},
		}}},
	}, nil); err != nil {
		return fmt.Errorf("letting administrators push All-Projects' config: %w", err)
	}
	return s.WriteAccess(ctx, "All-Projects", "Access as on AOS, plus the bridge's", AOSAllProjects,
		append(append([][]string{}, AllProjectsFor314...), BridgeAllProjects...))
}

// globalGroups are the system groups' UUIDs.
var globalGroups = map[string]string{
	"Anonymous Users":  "global:Anonymous-Users",
	"Registered Users": "global:Registered-Users",
	"Project Owners":   "global:Project-Owners",
	"Change Owner":     "global:Change-Owner",
}

var groupRef = regexp.MustCompile(`(?m)\bgroup (.+?)\s*$`)

// WriteAccess replaces project's project.config with config, edited by
// edits (arguments to `git config -f project.config`), and writes the
// groups file for the groups it names. Unchanged, it pushes nothing.
func (s *Server) WriteAccess(ctx context.Context, project, message, config string, edits [][]string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(s.Site), "meta-config-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	helper, env := gitmirror.HTTPCredentials(s.URL, "admin", s.Admin.Password)
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-c", helper[0]}, args...)...)
		cmd.Dir = tmp
		cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=Administrator", "GIT_AUTHOR_EMAIL=admin@example.com",
			"GIT_COMMITTER_NAME=Administrator", "GIT_COMMITTER_EMAIL=admin@example.com"), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, out)
		}
		return nil
	}
	remote := s.URL + "/a/" + project
	if err := git("init", "-q"); err != nil {
		return err
	}
	if err := git("fetch", "-q", remote, "refs/meta/config"); err != nil {
		return err
	}
	if err := git("checkout", "-q", "FETCH_HEAD"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "project.config"), []byte(config), 0o644); err != nil {
		return err
	}
	for _, e := range edits {
		if err := git(append([]string{"config", "-f", "project.config"}, e...)...); err != nil {
			return err
		}
	}
	written, err := os.ReadFile(filepath.Join(tmp, "project.config"))
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, m := range groupRef.FindAllStringSubmatch(string(written), -1) {
		names[m[1]] = true
	}
	var lines []string
	for name := range names {
		id, ok := globalGroups[name]
		if !ok {
			if id, err = s.groupID(ctx, name); err != nil {
				return fmt.Errorf("group %s: %w", name, err)
			}
		}
		lines = append(lines, fmt.Sprintf("%-40s\t%s", id, name))
	}
	sort.Strings(lines)
	groups := "# UUID                                  \tGroup Name\n#\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(tmp, "groups"), []byte(groups), 0o644); err != nil {
		return err
	}
	if err := git("add", "-A"); err != nil {
		return err
	}
	if err := git("diff", "--cached", "--quiet"); err == nil {
		return nil
	}
	if err := git("commit", "-q", "-m", message); err != nil {
		return err
	}
	return git("push", "-q", remote, "HEAD:refs/meta/config")
}
