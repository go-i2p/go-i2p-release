package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		// Publish unpushed commits first: go get resolves a peer by commit over the
		// network, so the tip we pin must already be on origin.
		if err := p.pushUnpushed(ctx); err != nil {
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
		if err := p.pushUnpushed(ctx); err != nil {
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
		if r.SkipReason != "" {
			continue
		}
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
		if r.SkipReason != "" {
			continue
		}
		if p.cfg.Sync && !r.Dirty {
			p.syncWorktree(ctx, r)
			if err := p.readState(ctx, r); err != nil {
				return err
			}
		}
	}

	var dirty, behind, mismatched, unresolved, skipped, unpushed []string
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if r.SkipReason != "" {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", name, r.SkipReason))
			p.journal.logf("  %-16s %-8s %s  (skipped: %s)", name, "-", "-", r.SkipReason)
			continue
		}
		if !r.RemoteBranch {
			unresolved = append(unresolved, name)
		}
		if r.Current != "" && r.Current != r.Branch {
			mismatched = append(mismatched, fmt.Sprintf("%s (%s -> %s)", name, r.Current, r.Branch))
		}
		if r.Dirty {
			dirty = append(dirty, name)
		}
		if r.Ahead > 0 {
			unpushed = append(unpushed, fmt.Sprintf("%s (%d)", name, r.Ahead))
		}
		if r.Behind > 0 {
			behind = append(behind, name)
		}
		if p.cfg.Quiet {
			continue
		}
		var notes []string
		if r.Ahead > 0 {
			notes = append(notes, fmt.Sprintf("%d unpushed", r.Ahead))
		}
		if r.Tip != r.LocalTip && r.Behind > 0 {
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

	if len(skipped) > 0 {
		p.journal.logf("  ! skipped (no git metadata): %s", strings.Join(skipped, ", "))
		p.journal.logf("    those modules are listed but never bumped, committed or tagged; run them from a real checkout")
	}

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
	if len(unpushed) > 0 {
		if p.readOnly || p.cfg.DryRun {
			p.journal.logf("  note: unpushed local commits in %s; a real run pushes them to origin before rewriting any go.mod",
				strings.Join(unpushed, ", "))
		} else {
			p.journal.logf("  ! unpushed local commits in %s", strings.Join(unpushed, ", "))
		}
	}
	if len(behind) > 0 {
		if p.cfg.DryRun || p.readOnly {
			p.journal.logf("  note: %s are behind origin; a real run fast-forwards them first", strings.Join(behind, ", "))
		} else {
			p.journal.logf("  ! still behind origin after sync: %s (diverged, or no remote branch)", strings.Join(behind, ", "))
		}
	}
	return nil
}

// workOrder returns the release order with skipped modules removed, so the
// mutating steps never touch a directory that is not a checkout.
func (s *Scan) workOrder() []string {
	var out []string
	for _, name := range s.Order {
		if s.Repos[name].SkipReason == "" {
			out = append(out, name)
		}
	}
	return out
}

// printScanWarnings reports discovery-level notes such as a dependency cycle.
func (p *Pipeline) printScanWarnings() {
	for _, w := range p.scan.Warnings {
		p.journal.logf("  ! %s", w)
	}
}

// modSnapshot holds the pre-bump contents of a repository's go.mod and go.sum, so
// a run that fails verification can put the tree back the way it found it.
type modSnapshot struct {
	// Names is the tracked path names in a stable order.
	Names []string
	Files map[string][]byte
	// HadFile records which paths existed before the bump, including those whose
	// content was empty, so a restore does not delete what it did not create.
	HadFile map[string]bool
}

// modFileNames are the files a dependency bump rewrites.
var modFileNames = []string{"go.mod", "go.sum"}

// snapshotFiles reads the module files from dir. Missing files are not an error:
// a module legitimately may have no go.sum.
func snapshotFiles(dir string) (*modSnapshot, error) {
	s := &modSnapshot{Names: modFileNames, Files: map[string][]byte{}, HadFile: map[string]bool{}}
	for _, name := range modFileNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s.Files[name] = data
		s.HadFile[name] = true
	}
	return s, nil
}

// restore puts the module files back exactly as recorded, deleting any that did not
// exist before. It returns the paths it touched.
func (s *modSnapshot) restore(dir string) []string {
	var touched []string
	for _, name := range s.Names {
		path := filepath.Join(dir, name)
		if !s.HadFile[name] {
			if _, err := os.Stat(path); err == nil {
				if os.Remove(path) == nil {
					touched = append(touched, name)
				}
			}
			continue
		}
		cur, err := os.ReadFile(path)
		if err == nil && bytes.Equal(cur, s.Files[name]) {
			continue
		}
		if os.WriteFile(path, s.Files[name], 0o644) == nil {
			touched = append(touched, name)
		}
	}
	return touched
}

// readState records the branch, HEAD, origin tip and dirty flag of a repository.
func (p *Pipeline) readState(ctx context.Context, r *Repo) error {
	head, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "HEAD")
	if err != nil {
		if !r.IsRepo {
			// Discovered by --all or --only: a module directory that is not a
			// checkout. It cannot be bumped or committed, so mark it and move on
			// rather than failing the whole namespace run.
			r.SkipReason = "not a git repository"
			return nil
		}
		return fmt.Errorf("%s: cannot read HEAD in %s: %w", r.Name, r.Path, err)
	}
	r.IsRepo = true
	r.Head = head
	branch, remote := p.detectBranch(ctx, r)
	r.branchDetected = true
	r.Branch = branch
	r.RemoteBranch = branch != "" && (remote == nil || remote[branch] != "")
	r.Current, _ = p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--abbrev-ref", "HEAD")

	// The local branch ref, not HEAD: unpushed work is measured on the branch that
	// would actually be pushed, so a checked-out feature branch or detached HEAD is
	// never mistaken for unpushed work on the release branch.
	if branch != "" {
		r.LocalTip, _ = p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/heads/"+branch)
	}
	// The tip is what origin holds right now, taken from the same ls-remote that
	// chose the branch. Using it rather than refs/remotes/origin/<branch> means a
	// stale fetch cannot make an unpushed local commit look like the tip. When
	// origin is unreachable we fall back to the remote-tracking ref, then the local
	// branch, so an offline run still plans against something.
	switch {
	case remote != nil && remote[branch] != "":
		r.Tip = remote[branch]
	default:
		tip, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/remotes/origin/"+branch)
		if err != nil || tip == "" {
			tip = r.LocalTip
		}
		r.Tip = tip
	}
	// Ahead/behind between the local branch and origin's tip. Ahead is the count of
	// commits go get cannot resolve yet, which is what the push-before-bump step acts
	// on; behind is origin commits the local branch is missing. The comparison object
	// must exist locally: with --sync the fetch put it there, but offline or with
	// --sync=false we fall back to the remote-tracking ref so unpushed work is still
	// detected against the last state we saw on origin.
	compare := r.Tip
	if compare != "" && r.LocalTip != "" {
		if _, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", compare+"^{commit}"); err != nil {
			if tr, terr := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/remotes/origin/"+branch); terr == nil && tr != "" {
				compare = tr
			}
		}
	}
	if r.LocalTip != "" && compare != "" && r.LocalTip != compare {
		if n, err := p.exec.Quiet(ctx, r.Path, "git", "rev-list", "--count", compare+".."+r.LocalTip); err == nil {
			r.Ahead = atoiDefault(n, 0)
		}
		if n, err := p.exec.Quiet(ctx, r.Path, "git", "rev-list", "--count", r.LocalTip+".."+compare); err == nil {
			r.Behind = atoiDefault(n, 0)
		}
	}
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

// remoteBranches lists the branches that actually exist on origin, mapped to the
// commit each points at. One network call per repository; a nil result means origin
// is unreachable (offline). The hash is the authoritative current tip, so it does
// not depend on how fresh the local remote-tracking refs are.
func (p *Pipeline) remoteBranches(ctx context.Context, r *Repo) map[string]string {
	if r.remotes != nil {
		return r.remotes
	}
	out, err := p.exec.Quiet(ctx, r.Path, "git", "ls-remote", "--heads", "origin")
	if err != nil {
		return nil
	}
	set := map[string]string{}
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if i := strings.LastIndex(fields[1], "/"); i >= 0 {
			set[fields[1][i+1:]] = fields[0]
		}
	}
	r.remotes = set
	return set
}

// detectBranch chooses the branch to release and push to. origin/HEAD is a local,
// possibly stale symbolic ref -- in go-i2p it names master while origin only has
// main -- so it is honoured only when the branch really exists on origin. Falling
// back to the checked-out branch last means a feature branch is never mistaken for
// a default. Returns "" when nothing can be confirmed, which blocks a writing run.
func (p *Pipeline) detectBranch(ctx context.Context, r *Repo) (string, map[string]string) {
	// Branch choice depends only on origin, so a resolved branch is stable across
	// the two inspect passes and the warning is reported once.
	if p.cfg.Branch == "" && r.branchDetected {
		return r.Branch, r.remotes
	}
	remote := p.remoteBranches(ctx, r)
	if p.cfg.Branch != "" {
		// An explicit --branch is trusted even when origin is unreachable, but a
		// known-existing remote must still contain it.
		if remote == nil || remote[p.cfg.Branch] != "" {
			return p.cfg.Branch, remote
		}
		p.journal.logf("  ! %s: --branch %s is not on origin", r.Name, p.cfg.Branch)
		return "", remote
	}

	if out, err := p.exec.Quiet(ctx, r.Path, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		name := strings.TrimPrefix(out, "origin/")
		if remote == nil || remote[name] != "" {
			return name, remote
		}
		p.journal.logf("  ! %s: origin/HEAD points at %s, which is not on origin; ignoring it", r.Name, name)
	}
	for _, c := range []string{"main", "master", "trunk"} {
		if remote[c] != "" {
			return c, remote
		}
	}
	// Nothing conventional: fall back to the checked-out branch only if origin has it.
	if cur, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil &&
		cur != "" && cur != "HEAD" && remote[cur] != "" {
		p.journal.logf("  ! %s: no main/master/trunk on origin; using checked-out branch %s", r.Name, cur)
		return cur, remote
	}
	return "", remote
}

// plan prints the resolved release set and what would change. The table is the
// command's deliverable, so it is written with printf and survives --quiet; the
// surrounding commentary is suppressed by it.
func (p *Pipeline) plan() {
	p.step("Release plan")
	p.journal.printf("%-16s %-8s %-9s %s", "REPOSITORY", "BRANCH", "TIP", "IN-NAMESPACE DEPENDENCIES")
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		deps := "-"
		if len(r.Deps) > 0 {
			deps = strings.Join(r.Deps, ", ")
		}
		status := ""
		if r.SkipReason != "" {
			status = "  (skipped: " + r.SkipReason + ")"
		}
		p.journal.printf("%-16s %-8s %-9s %s%s", name, r.Branch, short(r.Tip), deps, status)
	}
	if p.cfg.Version != "" {
		p.journal.printf("\ntag: %s (policy for existing tags: %s)", p.cfg.Version, p.cfg.TagExisting)
	}
	p.journal.printf("verify: %s, push: %v, dry-run: %v, sync: %v, standalone: %v, private: %v",
		p.cfg.Verify, p.cfg.Push, p.cfg.DryRun, p.cfg.Sync, p.cfg.Standalone, p.cfg.Private)
	if p.cfg.Push && !p.cfg.DryRun {
		p.journal.printf("note: plan resolves each repository's state but commits and pushes nothing; run `tip` or `release` to apply.")
	}
}

// preflightTags applies --tag-existing before any work is done, so a release that
// cannot tag cleanly stops before it commits or pushes anything.
func (p *Pipeline) preflightTags(ctx context.Context) error {
	// Namespace modules have no /vN path suffix, so a v2+ tag cannot be required by
	// `go get`. This is fatal whatever --tag-existing says, so it is checked before
	// the policy below is consulted.
	if mj, err := strconv.Atoi(strings.TrimPrefix(semver.Major(p.cfg.Version), "v")); err == nil && mj >= 2 {
		return fmt.Errorf("version %s requires a /v%d module path suffix in every repository (a semantic-import-versioning change this namespace does not use); release a v0.x or v1.x version instead",
			p.cfg.Version, mj)
	}
	if p.cfg.TagExisting != tagFail {
		return nil
	}

	var conflicts []string
	for _, name := range p.scan.Order {
		r := p.scan.Repos[name]
		if r.SkipReason != "" {
			continue
		}
		local, remote := p.tagLocations(ctx, r)
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

// tagLocations reports where the release tag exists. The remote side is read with
// ls-remote rather than refs/remotes/origin/<tag>: `git fetch` does not create
// remote-tracking refs for tags, so probing that ref misses any tag that exists
// only on the host and would be silently re-created and force-pushed over.
func (p *Pipeline) tagLocations(ctx context.Context, r *Repo) (local, remote string) {
	tagRef := "refs/tags/" + p.cfg.Version
	if out, err := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", tagRef); err == nil {
		local = out
	}
	out, err := p.exec.Quiet(ctx, r.Path, "git", "ls-remote", "--tags", "origin", tagRef)
	if err != nil {
		// origin unreachable: fall back to a remote-tracking ref if one happens to exist.
		if t, terr := p.exec.Quiet(ctx, r.Path, "git", "rev-parse", "--verify", "refs/remotes/origin/"+p.cfg.Version); terr == nil {
			remote = t
		}
		return local, remote
	}
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		// Annotated tags produce both refs/tags/v and refs/tags/v^{}; take the plain one.
		if len(fields) >= 2 && fields[1] == tagRef {
			remote = fields[0]
			break
		}
	}
	return local, remote
}

// pushUnpushed publishes local-only commits on each repository's branch to origin
// *before* any go.mod is rewritten. go get resolves a dependency by commit over the
// network, so a peer with unpushed work would otherwise be pinned to the older
// commit origin does have -- silently dropping that work from the release. Pushing
// first makes the tip we pin to the tip that is actually on the remote.
//
// It pushes the local branch ref (refs/heads/<branch>), never HEAD, so a checked-out
// feature branch is not leaked onto the release branch. Divergence (local commits
// plus missing origin commits) is refused rather than force-pushed over; a force
// push would rewrite shared history.
func (p *Pipeline) pushUnpushed(ctx context.Context) error {
	if p.readOnly {
		return nil
	}
	// Nothing to do unless some repository is ahead of origin.
	var pending []*Repo
	for _, name := range p.scan.workOrder() {
		if r := p.scan.Repos[name]; r.Ahead > 0 {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	p.step("Publishing unpushed commits before bumping")

	// Classify before publishing anything: a diverged repository must stop the whole
	// run, not be discovered after other repositories were already pushed.
	var blocked []string
	for _, r := range pending {
		switch {
		case r.Behind > 0:
			blocked = append(blocked, fmt.Sprintf("%s: %d local commit(s) and %d behind origin/%s (diverged)", r.Name, r.Ahead, r.Behind, r.Branch))
		case !r.RemoteBranch:
			blocked = append(blocked, fmt.Sprintf("%s: %d local commit(s) but %s is not on origin", r.Name, r.Ahead, r.Branch))
		}
	}
	if len(blocked) > 0 {
		if p.cfg.DryRun {
			p.journal.logf("  ! would refuse to bump: %s", strings.Join(blocked, "; "))
			p.journal.logf("    reconcile those repositories (pull --rebase, or push the branch to origin) before a real release")
			return nil
		}
		return fmt.Errorf("local commits cannot be published before bumping; reconcile each repository first (pull --rebase, or push its branch to origin):\n  %s",
			strings.Join(blocked, "\n  "))
	}

	for _, r := range pending {
		if p.cfg.DryRun {
			p.journal.logf("  (dry run) would push %d local commit(s) in %s to origin/%s", r.Ahead, r.Name, r.Branch)
			continue
		}
		if !p.cfg.Push {
			// The user asked not to publish anything. We cannot make the unpushed tip
			// resolvable, so pin dependents to the origin tip and say plainly that this
			// repository's local work is excluded.
			p.journal.logf("  ! %s: %d local commit(s) not on origin and --push=false; dependents pin to origin/%s@%s, excluding them",
				r.Name, r.Ahead, r.Branch, short(r.Tip))
			continue
		}
		if _, err := p.exec.Run(ctx, r.Path, false, "git", "push", "origin", "refs/heads/"+r.Branch+":"+r.Branch); err != nil {
			return fmt.Errorf("%s: pushing %d local commit(s) to origin/%s: %w", r.Name, r.Ahead, r.Branch, err)
		}
		// origin now holds the local tip; pin dependents to it so their go.get resolves
		// the commit that includes this repository's work.
		r.Tip = r.LocalTip
		p.journal.logf("  %s: pushed %d local commit(s) to origin/%s (%s)", r.Name, r.Ahead, r.Branch, short(r.Tip))
	}
	return nil
}

// bumpToTip points every in-namespace dependency at the tip of its default branch.
// Each repository's go.mod/go.sum are snapshotted first: if a dependency cannot be
// resolved, the module is put back exactly as it was found and the run stops before
// anything is committed, tagged or pushed. Without that, a failed `go get` would
// leave the tree rewritten to a local replace that was just dropped -- the state
// that makes a module unbuildable for everyone.
func (p *Pipeline) bumpToTip(ctx context.Context) error {
	p.step("Bumping dependencies to branch tips")
	// Use the origin tip, not local HEAD: go get resolves the commit over the
	// network, so an unpushed local commit would fail to fetch.
	return p.bump(ctx, func(peer *Repo) string { return p.moduleOf(peer) + "@" + peer.Tip }, true)
}

// bumpToTag rewrites dependencies to use the release tag instead of a pseudo-version.
func (p *Pipeline) bumpToTag(ctx context.Context) error {
	p.step("Rewriting dependencies to " + p.cfg.Version)
	return p.bump(ctx, func(peer *Repo) string { return p.moduleOf(peer) + "@" + p.cfg.Version }, false)
}

// bump applies resolve() to each in-namespace dependency of every selected
// repository. dropReplaces is used by the tip pass only: the tag pass follows a
// successful tip pass, where the replaces are already gone.
func (p *Pipeline) bump(ctx context.Context, resolve func(peer *Repo) string, dropReplaces bool) error {
	// Each pass starts from a clean slate: restore below must only unwind what this
	// pass snapshotted, never a previous pass whose commits are already in.
	for _, name := range p.scan.workOrder() {
		p.scan.Repos[name].Snapshot = nil
	}
	var broken []string
	for _, name := range p.scan.workOrder() {
		r := p.scan.Repos[name]
		if len(r.Deps) == 0 {
			continue
		}
		snap, err := snapshotFiles(r.Path)
		if err != nil {
			broken = append(broken, fmt.Sprintf("%s: %v", name, err))
			break
		}
		r.Snapshot = snap

		// A dependency with no resolvable tip (skipped, or never pushed) must not
		// rewrite this module at all, so check before editing anything.
		if unresolvable := p.unresolvedDeps(r); unresolvable != "" {
			p.journal.logf("  ! %s: cannot resolve %s; leaving it unchanged", name, unresolvable)
			broken = append(broken, fmt.Sprintf("%s (dependency %s)", name, unresolvable))
			break
		}

		if !p.cfg.Quiet {
			p.journal.logf("  %s: %s", name, strings.Join(r.Deps, ", "))
		}
		if err := p.bumpModule(ctx, r, resolve, dropReplaces); err != nil {
			broken = append(broken, fmt.Sprintf("%s: %v", name, err))
			break
		}
	}
	if len(broken) > 0 {
		// The whole pass is unwound, not just the module that broke: a partial bump
		// would leave some go.mod files rewritten and others not, and a re-run would
		// then snapshot the wrong baseline.
		p.restoreAll()
		return fmt.Errorf("could not update dependencies (%s); every go.mod/go.sum this pass rewrote was restored and nothing was committed, tagged or pushed",
			strings.Join(broken, ", "))
	}
	return nil
}

// bumpModule drops replaces, resolves each dependency and tidies one module. Any
// single failure aborts the whole pass, which the caller then restores.
func (p *Pipeline) bumpModule(ctx context.Context, r *Repo, resolve func(peer *Repo) string, dropReplaces bool) error {
	if dropReplaces {
		for _, dep := range r.Deps {
			peer := p.scan.Repos[dep]
			mod := p.moduleOf(peer)
			if _, err := p.exec.Run(ctx, r.Path, false, "go", "mod", "edit", "-dropreplace="+mod); err != nil {
				return fmt.Errorf("dropreplace %s: %v", mod, err)
			}
		}
	}
	for _, dep := range r.Deps {
		peer := p.scan.Repos[dep]
		target := resolve(peer)
		if _, err := p.exec.Run(ctx, r.Path, false, "go", "get", target); err != nil {
			return fmt.Errorf("go get %s: %v", target, err)
		}
	}
	if _, err := p.exec.Run(ctx, r.Path, false, "go", "mod", "tidy"); err != nil {
		return fmt.Errorf("go mod tidy: %v", err)
	}
	return nil
}

// unresolvedDeps names the dependencies of r that cannot be reached over origin.
func (p *Pipeline) unresolvedDeps(r *Repo) string {
	var missing []string
	for _, dep := range r.Deps {
		peer, ok := p.scan.Repos[dep]
		if !ok || peer.SkipReason != "" || peer.Tip == "" {
			missing = append(missing, dep)
		}
	}
	return strings.Join(missing, ", ")
}

// restore puts a repository's module files back the way they were found.
func (p *Pipeline) restore(r *Repo) {
	if r.Snapshot == nil {
		return
	}
	if touched := r.Snapshot.restore(r.Path); len(touched) > 0 && !p.cfg.Quiet {
		p.journal.logf("    restored %s in %s", strings.Join(touched, " and "), r.Name)
	}
}

// restoreAll unwinds every snapshot taken by the current pass.
func (p *Pipeline) restoreAll() {
	for _, name := range p.scan.workOrder() {
		p.restore(p.scan.Repos[name])
	}
}

// verify builds and optionally tests each module after the bumps. On failure it
// restores every go.mod/go.sum the run rewrote, so a broken tip never lands in a
// committed or pushed form.
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
	for _, name := range p.scan.workOrder() {
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
		p.restoreAll()
		return fmt.Errorf("verification failed for %s; the go.mod/go.sum rewritten by this pass were restored, so nothing was committed or pushed by it",
			strings.Join(failed, ", "))
	}
	return nil
}

// checkIn stages go.mod/go.sum and commits them in every repository that changed.
func (p *Pipeline) checkIn(ctx context.Context, message string) error {
	p.step("Checking in dependency updates")
	changed := 0
	for _, name := range p.scan.workOrder() {
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
		paths := changedModFiles(out)
		if len(paths) == 0 {
			continue
		}
		changed++
		r.HasPendingCommit = true
		p.journal.logf("  %s: commit %s", name, strings.Join(paths, "/"))
		// Stage and commit by explicit pathspec: a bare `git add go.mod go.sum` fails
		// atomically when go.sum is absent, and a bare `git commit` would then commit
		// whatever the user had already staged for a different change.
		add := append([]string{"git", "add", "--"}, paths...)
		if _, err := p.exec.Run(ctx, r.Path, false, add...); err != nil {
			p.journal.logf("    ! git add %s: %v", name, err)
			continue
		}
		commit := append([]string{"git", "commit", "-m", message, "--"}, paths...)
		if _, err := p.exec.Run(ctx, r.Path, false, commit...); err != nil {
			p.journal.logf("    ! git commit %s: %v", name, err)
		}
	}
	if changed == 0 && !p.cfg.Quiet {
		p.journal.logf("  no dependency changes to commit")
	}
	return nil
}

// changedModFiles lists the module files a `git status --porcelain -- go.mod go.sum`
// reports as changed. Only those are staged, so a missing go.sum cannot make the
// whole `git add` fail.
func changedModFiles(status string) []string {
	var out []string
	for _, line := range splitLines(status) {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[2:])
		// A rename or copy is reported as "old -> new"; the module files we care
		// about are on the right.
		if i := strings.LastIndex(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		if path == "go.mod" || path == "go.sum" {
			out = append(out, path)
		}
	}
	return out
}

// tag creates the annotated release tag in every repository.
// tag creates the annotated release tag in every repository. It runs after
// checkIn, so the tag lands on the dependency commit rather than the pre-bump one.
func (p *Pipeline) tag(ctx context.Context) error {
	p.step("Tagging repositories " + p.cfg.Version)
	for _, name := range p.scan.workOrder() {
		r := p.scan.Repos[name]
		local, remote := p.tagLocations(ctx, r)
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
	for _, name := range p.scan.workOrder() {
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
			for _, name := range p.scan.workOrder() {
				if u, ok := targets[name]; ok {
					p.journal.logf("    %-16s %s", name, u)
				}
			}
		}
	}
	for _, name := range p.scan.workOrder() {
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

// atoiDefault parses a count printed by `git rev-list --count`, falling back to def.
func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
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
