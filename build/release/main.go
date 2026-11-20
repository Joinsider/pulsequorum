// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

// main.go is the interactive part of `make release` (ADR 0040, amendment of
// 2026-11-20): it checks the local state, computes the next version, lets the
// project owner edit the changelog and creates a tag signed with the owner's
// local git configuration. No signing key ever lives in CI.

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// requiredChecks must have succeeded on the release commit (ci.yml job names).
var requiredChecks = []string{"go", "golangci", "web", "licenses", "generate", "workflow-lint", "secrets", "build"}

const (
	releaseBranch  = "master"
	allowedSigners = ".github/allowed_signers"
)

func main() {
	bump := flag.String("bump", "patch", "patch, minor or major")
	explicit := flag.String("version", "", "explicit version (e.g. 0.3.0 or 0.3.0-rc.1); wins over -bump")
	remote := flag.String("remote", "origin", "git remote to release from")
	dryRun := flag.Bool("dry-run", false, "only print version and changelog, create nothing")
	skipCI := flag.Bool("skip-ci-check", false, "do not require green CI checks (not recommended)")
	flag.Parse()

	if err := run(*bump, *explicit, *remote, *dryRun, *skipCI); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run(bump, explicit, remote string, dryRun, skipCI bool) error {
	if err := checkWorkingTree(remote); err != nil {
		return err
	}
	sha, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	tagsOut, err := git("tag", "--list", "v*")
	if err != nil {
		return err
	}
	latest := LatestTag(strings.Fields(tagsOut))
	next, err := NextVersion(latest, bump, explicit)
	if err != nil {
		return err
	}
	commits, err := commitsSince(latest)
	if err != nil {
		return err
	}
	notes := Changelog(commits)

	from := latest
	if from == "" {
		from = "(first release)"
	}
	fmt.Printf("Release %s from %s (previous: %s, %d commits)\n\n%s\n", next, short(sha), from, len(commits), notes)
	if dryRun {
		return nil
	}
	if !skipCI {
		if err := checkCI(sha); err != nil {
			return err
		}
	}

	msg, err := editMessage(next, notes)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(msg) }()
	// verbatim: git's default cleanup would drop the "### Features" headings as comments.
	if err := gitRun("tag", "--sign", "--cleanup=verbatim", "--file", msg, next, sha); err != nil {
		return fmt.Errorf("create signed tag: %w", err)
	}
	if err := gitRun("-c", "gpg.ssh.allowedSignersFile="+allowedSigners, "verify-tag", next); err != nil {
		_ = gitRun("tag", "--delete", next)
		return fmt.Errorf("tag signature does not match %s (tag deleted): %w", allowedSigners, err)
	}
	if !confirm(fmt.Sprintf("Push signed tag %s to %s? This starts the release workflow.", next, remote)) {
		fmt.Printf("Not pushed. Push later with: git push %s refs/tags/%s\n", remote, next)
		return nil
	}
	if err := gitRun("push", remote, "refs/tags/"+next); err != nil {
		return fmt.Errorf("push tag: %w", err)
	}
	fmt.Printf("Pushed %s. Review and publish the draft release once the workflow is green.\n", next)
	return nil
}

// checkWorkingTree requires a clean master that equals the remote master.
func checkWorkingTree(remote string) error {
	branch, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch != releaseBranch {
		return fmt.Errorf("releases are cut from %s, current branch is %s", releaseBranch, branch)
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("working tree is not clean")
	}
	if err := gitRun("fetch", "--quiet", "--tags", remote, releaseBranch); err != nil {
		return fmt.Errorf("fetch %s: %w", remote, err)
	}
	local, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	upstream, err := git("rev-parse", remote+"/"+releaseBranch)
	if err != nil {
		return err
	}
	if local != upstream {
		return fmt.Errorf("local %s (%s) differs from %s/%s (%s)", releaseBranch, short(local), remote, releaseBranch, short(upstream))
	}
	return nil
}

func commitsSince(latest string) ([]Commit, error) {
	rangeArg := "HEAD"
	if latest != "" {
		rangeArg = latest + "..HEAD"
	}
	out, err := git("log", "--no-merges", "--reverse", "--format=%H%x1f%s%x1f%b%x1e", rangeArg)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		parts := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x1f", 3)
		if len(parts) != 3 {
			continue
		}
		commits = append(commits, Commit{Hash: parts[0], Subject: parts[1], Body: strings.TrimSpace(parts[2])})
	}
	return commits, nil
}

// checkCI requires every required check run on sha to have concluded with success.
func checkCI(sha string) error {
	//nolint:gosec // G204: fixed binary, argv without shell; sha comes from git rev-parse.
	out, err := exec.Command("gh", "api", "--paginate", "--slurp",
		"repos/{owner}/{repo}/commits/"+sha+"/check-runs?per_page=100").Output()
	if err != nil {
		return fmt.Errorf("query check runs with gh (use -skip-ci-check only if CI is known green): %w", err)
	}
	var pages []struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return fmt.Errorf("decode check runs: %w", err)
	}
	ok := map[string]bool{}
	for _, p := range pages {
		for _, r := range p.CheckRuns {
			if r.Status == "completed" && r.Conclusion == "success" {
				ok[r.Name] = true
			}
		}
	}
	var missing []string
	for _, name := range requiredChecks {
		if !ok[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required checks not green on %s: %s", short(sha), strings.Join(missing, ", "))
	}
	return nil
}

// editMessage writes the tag message to a temp file and opens the editor.
func editMessage(version, notes string) (string, error) {
	f, err := os.CreateTemp("", "pulsequorum-release-*.md")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	header := "# Edit the release notes for " + version + ". Lines starting with '# ' are removed\n" +
		"# (Markdown headings such as '### Features' stay).\n" +
		"# The first line is the tag subject; everything after the blank line becomes the release notes.\n"
	if _, err := fmt.Fprintf(f, "%sPulsequorum %s\n\n%s", header, version, notes); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	editor := firstNonEmpty(os.Getenv("GIT_EDITOR"), os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	// The editor command comes from the local user's own environment, like git does it.
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "editor", f.Name()) //nolint:gosec // G204/G702: see above
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor: %w", err)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		return "", err
	}
	lines := slices.DeleteFunc(strings.Split(string(raw), "\n"), func(l string) bool { return l == "#" || strings.HasPrefix(l, "# ") })
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" {
		return "", errors.New("empty release notes, aborting")
	}
	return f.Name(), os.WriteFile(f.Name(), []byte(text+"\n"), 0o600) //nolint:gosec // G703: path from os.CreateTemp
}

func confirm(question string) bool {
	fmt.Printf("%s [y/N] ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(answer), "y")
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output() //nolint:gosec // G204: fixed binary, argv without shell
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitRun(args ...string) error {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed binary, argv without shell
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
