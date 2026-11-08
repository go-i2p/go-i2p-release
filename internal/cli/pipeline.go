package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Pipeline executes the release workflow against a scanned set of repositories.
type Pipeline struct {
	cfg      *Config
	scan     *Scan
	journal  *Journal
	exec     *Executor
	readOnly bool // set for the plan/check mode, which changes nothing
}

// NewPipeline returns a pipeline for cfg over the discovered scan.
func NewPipeline(cfg *Config, scan *Scan, j *Journal) *Pipeline {
	return &Pipeline{cfg: cfg, scan: scan, journal: j, exec: NewExecutor(cfg, j)}
}

// SetReadOnly marks the pipeline as inspect-only, so findings such as dirty trees
// are reported instead of aborting the run.
func (p *Pipeline) SetReadOnly() { p.readOnly = true }

// Run performs the requested mode.
func (p *Pipeline) Run(ctx context.Context, mode string) error {
	if err := p.inspect(ctx); err != nil {
		return err
	}
	switch mode {
	case modeCheck:
		p.plan()
		return nil
	case modeTip:
		if err := p.bumpToTip(ctx); err != nil {
			return err
		}
		if err := p.verify(ctx); err != nil {
			return err
		}
		if err := p.checkIn(ctx, p.cfg.Message); err != nil {
			return err
		}
		return p.publish(ctx, false)
	case modeRelease:
		if p.cfg.Version == "" {
			return fmt.Errorf("a release version is required")
		}
		// Fail before writing anything if an existing tag violates the policy, rather
		// than after commits are already checked in and pushed.
		if err := p.preflightTags(ctx); err != nil {
			return err
		}
		if err := p.bumpToTip(ctx); err != nil {
			return err
		}
		if err := p.verify(ctx); err != nil {
			return err
		}
		if err := p.checkIn(ctx, p.cfg.Message); err != nil {
			return err
		}
		if err := p.tag(ctx); err != nil {
			return err
		}
		if err := p.publish(ctx, true); err != nil {
			return err
		}
		if !p.cfg.PostTagBump {
			return nil
		}
		// Second pass: depend on the tag rather than a pseudo-version, then check in
		// and push again so the released tree references releases.
		if err := p.bumpToTag(ctx); err != nil {
			return err
		}
		if err := p.verify(ctx); err != nil {
			return err
		}
		if err := p.checkIn(ctx, "Update dependencies to "+p.cfg.Version); err != nil {
			return err
		}
		return p.publish(ctx, false)
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
}

// inspect resolves each repository's default branch, HEAD, origin tip and dirty
// state, and brings clean working trees up to the tip when --sync is set.
func (p *Pipeline) inspect(ctx context.Context) error {
	p.step("Inspecting %d repositories in %s", len(p.scan.Repos), p.cfg.Namespace)
	if gw := FindGoWork(p.cfg.Namespace); gw != "" {
		if p.cfg.Standalone {
			p.journal.logf("  go.work at %s; resolving each module standalone (GOWORK=off)", gw)
		} else {
			p.journal.logf("  ! go.work at %s is active and does not list every module; build ./... fails in the others. Use --standalone=true.", gw)
		}
	}

	// Pass 1: read state. Fetch only updates remote-tracking refs, so it is safe
	// even in a dry run; the plan should reflect what origin actually holds.
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if p.cfg.Sync {
			if _, err := p.exec.Run(ctx, r.Path, true, "git", "fetch", "--prune", "origin"); err != nil {
				p.journal.logf("  ! %s: fetch failed: %v", name, err)
			}
		}
		if err := p.readState(ctx, r); err != nil {
			return err
		}
	}

	// Pass 2: move clean trees onto their tip, then re-read.
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if p.cfg.Sync && !r.Dirty {
			p.syncWorktree(ctx, r)
			if err := p.readState(ctx, r); err != nil {
				return err
			}
		}
	}

	var dirty, behind, mismatched, unresolved []string
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if !r.RemoteBranch {
			unresolved = append(unresolved, name)
		}
		if r.Current != "" && r.Current != r.Branch {
			mismatched = append(mismatched, fmt.Sprintf("%s (%s -> %s)", name, r.Current, r.Branch))
		}
		if r.Dirty {
			dirty = append(dirty, name)
		}
		if r.Tip != r.Head {
			behind = append(behind, name)
		}
		if p.cfg.Quiet {
			continue
		}
		var notes []string
		if r.Tip != r.Head {
			notes = append(notes, "origin/"+r.Branch+"@"+short(r.Tip))
		}
		if r.Dirty {
			notes = append(notes, "dirty")
		}
		if len(r.Untracked) > 0 {
			notes = append(notes, fmt.Sprintf("%d untracked", len(r.Untracked)))
		}
		if r.Current != "" && r.Current != r.Branch {
			notes = append(notes, "checked out on "+r.Current)
		}
		suffix := ""
		if len(notes) > 0 {
			suffix = "  (" + strings.Join(notes, ", ") + ")"
		}
		p.journal.logf("  %-16s %-8s %s%s", name, r.Branch, short(r.Head), suffix)
		for _, w := range r.Warnings {
			p.journal.logf("    ! %s", w)
		}
	}

	// Report structural findings before failing, so a cycle warning is not hidden
	// behind a dirty-tree error.
	p.printScanWarnings()

	if len(unresolved) > 0 {
		p.journal.logf("  ! no default branch could be confirmed on origin for: %s", strings.Join(unresolved, ", "))
		if !p.readOnly && !p.cfg.DryRun {
			return fmt.Errorf("cannot confirm each repository's default branch on origin (missing for: %s); refusing to guess a branch to push to. Pass --branch to name it",
				strings.Join(unresolved, ", "))
		}
	}

	if len(mismatched) > 0 {
		p.journal.logf("  note: releasing to each repository's default branch, not the checked-out branch: %s",
			strings.Join(mismatched, ", "))
	}

	if len(dirty) > 0 && !p.cfg.AllowDirty {
		if p.readOnly || p.cfg.DryRun {
			// Nothing is written in these modes, so report and carry on: the point of
			// a dry run is to preview the whole plan.
			p.journal.logf("  note: uncommitted changes in %s; a real run will refuse unless you pass --allow-dirty",
				strings.Join(dirty, ", "))
		} else {
			var detail []string
			for _, n := range dirty {
				f := p.scan.Repos[n].DirtyFiles
				if len(f) > 3 {
					f = append(f[:3:3], fmt.Sprintf("... (%d more)", len(f)-3))
				}
				detail = append(detail, fmt.Sprintf("%s: %s", n, strings.Join(f, ", ")))
			}
			return fmt.Errorf("uncommitted changes; commit or stash them, or pass --allow-dirty\n  %s",
				strings.Join(detail, "\n  "))
		}
	}
	if len(behind) > 0 {
		if p.cfg.DryRun || p.readOnly {
			p.journal.logf("  note: %s differ from origin; a real run fast-forwards them first", strings.Join(behind, ", "))
		} else {
			p.journal.logf("  ! still behind origin after sync: %s (diverged, or no remote branch)", strings.Join(behind, ", "))
		}
	}
	return nil
}

// printScanWarnings reports discovery-level notes such as a dependency cycle.
func (p *Pipeline) printScanWarnings() {
	for _, w := range p.scan.Warnings {
		p.journal.logf("  ! %s", w)
	}
}

// readState records the branch, HEAD, origin tip and dirty flag of a repository.
func (p *Pipeline) readState(ctx context.Context, r *Repo) error {
	head, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("%s: cannot read HEAD in %s: %w", r.Name, r.Path, err)
	}
	r.Head = head
	branch, remote := p.detectBranch(ctx, r)
	r.branchDetected = true
	r.Branch = branch
	r.RemoteBranch = branch != "" && (remote == nil || remote[branch])
	tip, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/remotes/origin/"+r.Branch)
	if err != nil {
		// Unknown remote branch: offline, or the repository was never pushed.
		tip = head
	}
	r.Tip = tip
	r.Current, _ = p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--abbrev-ref", "HEAD")
	// Stale stat data (a freshly copied worktree, for example) can make git report
	// phantom modifications, so refresh the index before asking for status.
	p.exec.Quiet(ctx, r.Path, "git", "update-index", "--refresh")

	// Only tracked modifications block a run: the tool commits go.mod/go.sum, and
	// unrelated untracked files must not stop a namespace-wide release.
	tracked, err := p.exec.Quiet(ctx, r.Path, "git", "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fmt.Errorf("%s: git status: %w", r.Name, err)
	}
	r.Dirty = strings.TrimSpace(tracked) != ""
	r.DirtyFiles = splitLines(tracked)

	untracked, _ := p.exec.Quiet(ctx, r.Path, "git", "status", "--porcelain", "--untracked-files=normal")
	r.Untracked = nil
	for _, line := range splitLines(untracked) {
		if strings.HasPrefix(line, "?? ") {
			r.Untracked = append(r.Untracked, strings.TrimPrefix(line, "?? "))
		}
	}
	return nil
}

// syncWorktree brings a clean working tree onto the tip of its default branch.
// Detached or diverged trees are reported and left alone rather than reset.
func (p *Pipeline) syncWorktree(ctx context.Context, r *Repo) {
	if p.cfg.DryRun || p.readOnly || r.Dirty || !r.RemoteBranch {
		return
	}
	branch := r.Branch
	current, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return
	}
	if current != branch {
		if _, err := p.exec.Run(ctx, r.Path, false, "git", "checkout", branch); err != nil {
			p.journal.logf("  ! %s: checkout %s: %v", r.Name, branch, err)
			return
		}
	}
	if _, err := p.exec.Run(ctx, r.Path, false, "git", "merge", "--ff-only", "origin/"+branch); err != nil {
		p.journal.logf("  ! %s: fast-forward to origin/%s: %v", r.Name, branch, err)
	}
}

// remoteBranches lists the branches that actually exist on origin. One network
// call per repository; an empty result means origin is unreachable (offline).
func (p *Pipeline) remoteBranches(ctx context.Context, r *Repo) map[string]bool {
	if r.remotes != nil {
		return r.remotes
	}
	out, err := p.exec.Quiet(ctx, r.Path, "git", "ls-remote", "--heads", "origin")
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if i := strings.LastIndex(fields[1], "/"); i >= 0 {
			set[fields[1][i+1:]] = true
		}
	}
	if err == nil {
		r.remotes = set
	}
	return set
}

// detectBranch chooses the branch to release and push to. origin/HEAD is a local,
// possibly stale symbolic ref -- in go-i2p it names master while origin only has
// main -- so it is honoured only when the branch really exists on origin. Falling
// back to the checked-out branch last means a feature branch is never mistaken for
// a default. Returns "" when nothing can be confirmed, which blocks a writing run.
func (p *Pipeline) detectBranch(ctx context.Context, r *Repo) (string, map[string]bool) {
	// Branch choice depends only on origin, so a resolved branch is stable across
	// the two inspect passes and the warning is reported once.
	if p.cfg.Branch == "" && r.branchDetected {
		return r.Branch, r.remotes
	}
	remote := p.remoteBranches(ctx, r)
	if p.cfg.Branch != "" {
		// An explicit --branch is trusted even when origin is unreachable, but a
		// known-existing remote must still contain it.
		if remote == nil || remote[p.cfg.Branch] {
			return p.cfg.Branch, remote
		}
		p.journal.logf("  ! %s: --branch %s is not on origin", r.Name, p.cfg.Branch)
		return "", remote
	}

	if out, err := p.exec.Quiet(ctx, r.Path, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		name := strings.TrimPrefix(out, "origin/")
		if remote == nil || remote[name] {
			return name, remote
		}
		p.journal.logf("  ! %s: origin/HEAD points at %s, which is not on origin; ignoring it", r.Name, name)
	}
	for _, c := range []string{"main", "master", "trunk"} {
		if remote[c] {
			return c, remote
		}
	}
	// Nothing conventional: fall back to the checked-out branch only if origin has it.
	if cur, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil &&
		cur != "" && cur != "HEAD" && remote[cur] {
		p.journal.logf("  ! %s: no main/master/trunk on origin; using checked-out branch %s", r.Name, cur)
		return cur, remote
	}
	return "", remote
}

// plan prints the resolved release set and what would change.
func (p *Pipeline) plan() {
	p.step("Release plan")
	p.journal.logf("%-16s %-8s %-9s %s", "REPOSITORY", "BRANCH", "TIP", "IN-NAMESPACE DEPENDENCIES")
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		deps := "-"
		if len(r.Deps) > 0 {
			deps = strings.Join(r.Deps, ", ")
		}
		p.journal.logf("%-16s %-8s %-9s %s", name, r.Branch, short(r.Tip), deps)
	}
	if p.cfg.Version != "" {
		p.journal.logf("\ntag: %s (policy for existing tags: %s)", p.cfg.Version, p.cfg.TagExisting)
	}
	p.journal.logf("verify: %s, push: %v, dry-run: %v, sync: %v, standalone: %v, private: %v",
		p.cfg.Verify, p.cfg.Push, p.cfg.DryRun, p.cfg.Sync, p.cfg.Standalone, p.cfg.Private)
}

// preflightTags applies --tag-existing before any work is done, so a release that
// cannot tag cleanly stops before it commits or pushes anything.
func (p *Pipeline) preflightTags(ctx context.Context) error {
	if p.cfg.TagExisting != tagFail {
		return nil
	}
	// Namespace modules have no /vN path suffix, so a v2+ tag cannot be required by
	// `go get`. Catch that here instead of after the tags are pushed.
	if mj, err := strconv.Atoi(strings.TrimPrefix(semver.Major(p.cfg.Version), "v")); err == nil && mj >= 2 {
		return fmt.Errorf("version %s requires a /v%d module path suffix in every repository (a semantic-import-versioning change this namespace does not use); release a v0.x or v1.x version instead",
			p.cfg.Version, mj)
	}

	var conflicts []string
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		local, _ := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/tags/"+p.cfg.Version)
		remote, _ := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/remotes/origin/"+p.cfg.Version)
		if local != "" || remote != "" {
			conflicts = append(conflicts, fmt.Sprintf("%s (%s)", name, where(local, remote)))
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("tag %s already exists in %s; nothing was changed. Pass --tag-existing=recreate to move it or --tag-existing=skip to leave it",
			p.cfg.Version, strings.Join(conflicts, ", "))
	}
	return nil
}

// bumpToTip points every in-namespace dependency at the tip of its default branch.
func (p *Pipeline) bumpToTip(ctx context.Context) error {
	p.step("Bumping dependencies to branch tips")
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if len(r.Deps) == 0 {
			continue
		}
		if !p.cfg.Quiet {
			p.journal.logf("  %s: %s", name, strings.Join(r.Deps, ", "))
		}
		// Drop local replaces so the published graph matches what gets tagged.
		for _, dep := range r.Deps {
			peer := p.scan.Repos[dep]
			if _, err := p.exec.Run(ctx, r.Path, false, "go", "mod", "edit", "-dropreplace="+p.moduleOf(peer)); err != nil {
				p.journal.logf("    ! dropreplace %s in %s: %v", dep, name, err)
			}
		}
		for _, dep := range r.Deps {
			peer := p.scan.Repos[dep]
			// Use the origin tip, not local HEAD: go get resolves the commit over the
			// network, so an unpushed local commit would fail to fetch.
			target := p.moduleOf(peer) + "@" + peer.Tip
			if _, err := p.exec.Run(ctx, r.Path, false, "go", "get", target); err != nil {
				p.journal.logf("    ! go get %s in %s: %v", target, name, err)
			}
		}
		if _, err := p.exec.Run(ctx, r.Path, false, "go", "mod", "tidy"); err != nil {
			p.journal.logf("    ! go mod tidy in %s: %v", name, err)
		}
	}
	return nil
}

// bumpToTag rewrites dependencies to use the release tag instead of a pseudo-version.
func (p *Pipeline) bumpToTag(ctx context.Context) error {
	p.step("Rewriting dependencies to " + p.cfg.Version)
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if len(r.Deps) == 0 {
			continue
		}
		for _, dep := range r.Deps {
			peer := p.scan.Repos[dep]
			target := p.moduleOf(peer) + "@" + p.cfg.Version
			if _, err := p.exec.Run(ctx, r.Path, false, "go", "get", target); err != nil {
				p.journal.logf("    ! go get %s in %s: %v", target, name, err)
			}
		}
		if _, err := p.exec.Run(ctx, r.Path, false, "go", "mod", "tidy"); err != nil {
			p.journal.logf("    ! go mod tidy in %s: %v", name, err)
		}
	}
	return nil
}

// verify builds and optionally tests each module after the bumps.
func (p *Pipeline) verify(ctx context.Context) error {
	if p.cfg.Verify == verifyNone {
		return nil
	}
	if p.cfg.DryRun {
		p.step("Verifying modules")
		cmds := "go build ./..."
		if p.cfg.Verify == verifyTest {
			cmds += " then go test ./..."
		}
		p.journal.logf("  (dry run) would run %s in each repository", cmds)
		return nil
	}
	label := "go build ./..."
	if p.cfg.Verify == verifyTest {
		label += " + go test ./..."
	}
	p.step("Verifying modules with " + label)
	var failed []string
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if _, err := p.exec.Run(ctx, r.Path, false, "go", "build", "./..."); err != nil {
			failed = append(failed, name+" (build)")
			p.journal.logf("  ! build %s: %v", name, err)
			continue
		}
		if p.cfg.Verify == verifyTest {
			if _, err := p.exec.Run(ctx, r.Path, false, "go", "test", "./..."); err != nil {
				failed = append(failed, name+" (test)")
				p.journal.logf("  ! test %s: %v", name, err)
				continue
			}
		}
		if !p.cfg.Quiet {
			p.journal.logf("  ok %s", name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("verification failed for %s; nothing was committed or tagged", strings.Join(failed, ", "))
	}
	return nil
}

// checkIn stages go.mod/go.sum and commits them in every repository that changed.
func (p *Pipeline) checkIn(ctx context.Context, message string) error {
	p.step("Checking in dependency updates")
	changed := 0
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		out, err := p.exec.Quiet(ctx, r.Path, "git", "status", "--porcelain", "--", "go.mod", "go.sum")
		if err != nil {
			p.journal.logf("  ! %s: git status: %v", name, err)
			continue
		}
		if strings.TrimSpace(out) == "" {
			if !p.cfg.Quiet {
				p.journal.logf("  %s: already up to date", name)
			}
			continue
		}
		changed++
		r.HasPendingCommit = true
		p.journal.logf("  %s: commit go.mod/go.sum (%s)", name, strings.Join(strings.Fields(out), " "))
		if _, err := p.exec.Run(ctx, r.Path, false, "git", "add", "go.mod", "go.sum"); err != nil {
			p.journal.logf("    ! git add %s: %v", name, err)
		}
		if _, err := p.exec.Run(ctx, r.Path, false, "git", "commit", "-m", message); err != nil {
			p.journal.logf("    ! git commit %s: %v", name, err)
		}
	}
	if changed == 0 && !p.cfg.Quiet {
		p.journal.logf("  no dependency changes to commit")
	}
	return nil
}

// tag creates the annotated release tag in every repository.
// tag creates the annotated release tag in every repository. It runs after
// checkIn, so the tag lands on the dependency commit rather than the pre-bump one.
func (p *Pipeline) tag(ctx context.Context) error {
	p.step("Tagging repositories " + p.cfg.Version)
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		local, _ := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/tags/"+p.cfg.Version)
		remote, _ := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/remotes/origin/"+p.cfg.Version)
		if local != "" || remote != "" {
			switch p.cfg.TagExisting {
			case tagSkip:
				p.journal.logf("  %s: %s already exists (%s); leaving it", name, p.cfg.Version, where(local, remote))
				continue
			case tagFail:
				return fmt.Errorf("%s: tag %s already exists (%s); pass --tag-existing=recreate to move it or --tag-existing=skip to leave it",
					name, p.cfg.Version, where(local, remote))
			case tagRecreate:
				p.journal.logf("  %s: recreating %s (%s)", name, p.cfg.Version, where(local, remote))
				if local != "" {
					if _, err := p.exec.Run(ctx, r.Path, false, "git", "tag", "-d", p.cfg.Version); err != nil {
						p.journal.logf("    ! delete local tag in %s: %v", name, err)
					}
				}
				if remote != "" && p.cfg.Push {
					if _, err := p.exec.Run(ctx, r.Path, false, "git", "push", "origin", "--delete", "refs/tags/"+p.cfg.Version); err != nil {
						p.journal.logf("    ! delete remote tag in %s: %v", name, err)
					}
				}
			}
		}
		if _, err := p.exec.Run(ctx, r.Path, false, "git", "tag", "-a", p.cfg.Version, "-m", "Release "+p.cfg.Version); err != nil {
			p.journal.logf("    ! tag %s in %s: %v", p.cfg.Version, name, err)
		}
	}
	return nil
}

// publish pushes branches and, when tagging, the release tag.
func (p *Pipeline) publish(ctx context.Context, withTag bool) error {
	if !p.cfg.Push {
		p.step("Leaving changes local")
		p.journal.logf("  not pushing (use --push to publish)")
		return nil
	}
	p.step("Pushing repositories to origin")
	// Show the actual destination before publishing: a namespace copied elsewhere,
	// or a repository whose remote was re-pointed, should be visible in the log.
	targets := map[string]string{}
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if url, err := p.exec.Quiet(ctx, r.Path, "git", "remote", "get-url", "origin"); err == nil {
			targets[name] = url
		}
	}
	if !p.cfg.Quiet && len(targets) > 0 {
		unique := map[string]bool{}
		for _, u := range targets {
			unique[u] = true
		}
		if len(unique) == 1 {
			for u := range unique {
				p.journal.logf("  remote: %s", u)
			}
		} else {
			p.journal.logf("  remotes differ per repository; verify before publishing:")
			for _, name := range p.scan.Order {
				if u, ok := targets[name]; ok {
					p.journal.logf("    %-16s %s", name, u)
				}
			}
		}
	}
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		// Pushing an unconfirmed branch would create it on the remote, which is how a
		// stale origin/HEAD turns into a bogus new branch.
		if !r.RemoteBranch {
			p.journal.logf("  ! %s: skipping push; branch %q is not confirmed on origin", name, r.Branch)
			continue
		}
		// Skip the branch push when this run committed nothing: there is nothing of
		// ours to publish, and pushing someone else's unrelated local work is not.
		if !r.HasPendingCommit {
			if !p.cfg.Quiet {
				p.journal.logf("  %s: nothing to push", name)
			}
		} else if _, err := p.exec.Run(ctx, r.Path, false, "git", "push", "origin", "HEAD:"+r.Branch); err != nil {
			p.journal.logf("  ! push %s: %v", name, err)
		}
		if withTag && p.cfg.Version != "" {
			if _, err := p.exec.Run(ctx, r.Path, false, "git", "push", "origin", "refs/tags/"+p.cfg.Version); err != nil {
				p.journal.logf("  ! push tag %s in %s: %v", p.cfg.Version, name, err)
			}
		}
	}
	return nil
}

func (p *Pipeline) moduleOf(r *Repo) string {
	if r.Module != "" {
		return r.Module
	}
	return p.cfg.Prefix + "/" + r.Name
}

func (p *Pipeline) step(format string, args ...any) {
	if p.cfg.Quiet {
		return
	}
	p.journal.logf("\n=== "+format+" ===", args...)
}

// Summary renders the tail-of-run statistics.
func (p *Pipeline) Summary(start time.Time) string {
	mode := "tip check-in (no tags)"
	if p.cfg.Version != "" {
		mode = "release " + p.cfg.Version
	}
	if p.cfg.DryRun {
		mode += " [dry run]"
	}
	fails := p.journal.Failures()
	lines := []string{fmt.Sprintf("%s: %d repositories, %d actions, %d planned, %d failed, %s",
		"go-i2p-release", len(p.scan.Repos), len(p.journal.Actions), len(p.journal.Planned()), len(fails),
		time.Since(start).Round(time.Millisecond))}
	if len(fails) > 0 {
		lines = append(lines, fmt.Sprintf("  first failure: %s", fails[0]))
	}
	switch {
	case p.readOnly:
		// plan inspects only; the dry-run note would be misleading.
	case p.cfg.DryRun:
		lines = append(lines, "  dry run: re-run with --dry-run=false to apply")
	}
	return strings.Join(lines, "\n")
}

// short abbreviates a commit hash for display.
func short(hash string) string {
	if hash == "" {
		return "-"
	}
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

// where describes which side an existing tag was found on.
func where(local, remote string) string {
	switch {
	case local != "" && remote != "":
		return "local and remote"
	case local != "":
		return "local"
	case remote != "":
		return "remote"
	default:
		return "absent"
	}
}
