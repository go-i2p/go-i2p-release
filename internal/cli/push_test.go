package cli

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the guarantee that a dependency's local-only commits reach
// origin before any go.mod is pinned to them: `go get <peer>@<hash>` resolves over
// the network, so pinning to an unpushed commit would fail, and pinning to the older
// origin commit would silently drop that work from the release.

// originTip reads the commit origin currently holds for a branch.
func originTip(t *testing.T, dir, branch string) string {
	t.Helper()
	out := githelp(t, dir, "ls-remote", "origin", "refs/heads/"+branch)
	fields := strings.Fields(out)
	if len(fields) < 1 {
		t.Fatalf("origin has no refs/heads/%s (ls-remote: %q)", branch, out)
	}
	return fields[0]
}

// TestReadStateMeasuresUnpushedOnBranchRef checks that ahead/behind are computed
// against the branch that would be pushed, using the authoritative origin tip.
func TestReadStateMeasuresUnpushedOnBranchRef(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	dep := filepath.Join(base, "dep")
	ctx := context.Background()

	newCheckout(t, dep, "main")
	githelp(t, dep, "init", "--quiet", "--bare", "-b", "main", origin)
	githelp(t, dep, "remote", "add", "origin", origin)
	githelp(t, dep, "push", "-q", "-u", "origin", "main")
	pushed := originTip(t, dep, "main")

	// One local-only commit.
	githelp(t, dep, "commit", "-q", "--allow-empty", "-m", "local-only")
	local := githelp(t, dep, "rev-parse", "refs/heads/main")

	cfg := Defaults()
	cfg.Namespace = base
	cfg.Sync = false
	r := &Repo{Name: "dep", Path: dep, IsRepo: true}
	j := NewJournal(io.Discard)
	p := NewPipeline(cfg, &Scan{Repos: map[string]*Repo{"dep": r}, Order: []string{"dep"}}, j)

	if err := p.readState(ctx, r); err != nil {
		t.Fatalf("readState: %v", err)
	}
	if r.Branch != "main" {
		t.Errorf("Branch = %q, want main", r.Branch)
	}
	// The tip is what origin holds, not the local commit.
	if r.Tip != pushed {
		t.Errorf("Tip = %s, want origin tip %s", short(r.Tip), short(pushed))
	}
	if r.LocalTip != local {
		t.Errorf("LocalTip = %s, want local branch %s", short(r.LocalTip), short(local))
	}
	if r.Ahead != 1 {
		t.Errorf("Ahead = %d, want 1", r.Ahead)
	}
	if r.Behind != 0 {
		t.Errorf("Behind = %d, want 0", r.Behind)
	}

	// A push must publish the branch ref and refresh the pin target.
	cfg.Push = true
	cfg.DryRun = false
	if err := p.pushUnpushed(ctx); err != nil {
		t.Fatalf("pushUnpushed: %v", err)
	}
	if got := originTip(t, dep, "main"); got != local {
		t.Errorf("origin tip after push = %s, want %s", short(got), short(local))
	}
	if r.Tip != local {
		t.Errorf("Tip = %s after push, want %s so dependents pin to the published commit", short(r.Tip), short(local))
	}
}

// TestPushUnpushedRefusesDivergence: local commits plus missing origin commits mean
// a plain push is rejected and a force push would rewrite shared history. The run
// must stop before bumping rather than drop either side.
func TestPushUnpushedRefusesDivergence(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	dep := filepath.Join(base, "dep")
	ctx := context.Background()

	newCheckout(t, dep, "main")
	githelp(t, dep, "init", "--quiet", "--bare", "-b", "main", origin)
	githelp(t, dep, "remote", "add", "origin", origin)
	githelp(t, dep, "push", "-q", "-u", "origin", "main")

	// Advance origin from a separate clone so dep is behind without knowing it.
	other := filepath.Join(base, "other")
	githelp(t, base, "clone", "-q", origin, "other")
	githelp(t, other, "config", "user.email", "t@example.com")
	githelp(t, other, "config", "user.name", "t")
	githelp(t, other, "commit", "-q", "--allow-empty", "-m", "on-origin")
	githelp(t, other, "push", "-q", "origin", "main")

	// And give dep its own unpushed commit, so it is both ahead and behind.
	githelp(t, dep, "commit", "-q", "--allow-empty", "-m", "local-only")
	// Fetch so dep can actually see both sides of the divergence, the way a
	// --sync run does before pushUnpushed measures it.
	githelp(t, dep, "fetch", "-q", "origin")

	cfg := Defaults()
	cfg.Namespace = base
	cfg.Sync = false
	r := &Repo{Name: "dep", Path: dep, IsRepo: true}
	j := NewJournal(io.Discard)
	p := NewPipeline(cfg, &Scan{Repos: map[string]*Repo{"dep": r}, Order: []string{"dep"}}, j)

	if err := p.readState(ctx, r); err != nil {
		t.Fatalf("readState: %v", err)
	}
	if r.Ahead == 0 || r.Behind == 0 {
		t.Fatalf("expected divergence, got ahead=%d behind=%d", r.Ahead, r.Behind)
	}
	err := p.pushUnpushed(ctx)
	if err == nil {
		t.Fatal("diverged repository must be refused before bumping")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("error should mention divergence, got: %v", err)
	}
	// Nothing was pushed: origin still holds only the other clone's commit.
	if got := originTip(t, dep, "main"); got == r.LocalTip {
		t.Error("diverged branch must not be pushed")
	}
}

// TestPushUnpushedHonoursNoPush: with --push=false the tool cannot make an unpushed
// tip resolvable, so it must not force anything; it reports that the work is excluded
// and leaves the pin at the origin tip.
func TestPushUnpushedHonoursNoPush(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	dep := filepath.Join(base, "dep")
	ctx := context.Background()

	newCheckout(t, dep, "main")
	githelp(t, dep, "init", "--quiet", "--bare", "-b", "main", origin)
	githelp(t, dep, "remote", "add", "origin", origin)
	githelp(t, dep, "push", "-q", "-u", "origin", "main")
	pushed := originTip(t, dep, "main")
	githelp(t, dep, "commit", "-q", "--allow-empty", "-m", "local-only")

	var buf strings.Builder
	cfg := Defaults()
	cfg.Namespace = base
	cfg.Sync = false
	cfg.Push = false
	r := &Repo{Name: "dep", Path: dep, IsRepo: true}
	j := NewJournal(&buf)
	p := NewPipeline(cfg, &Scan{Repos: map[string]*Repo{"dep": r}, Order: []string{"dep"}}, j)

	if err := p.readState(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := p.pushUnpushed(ctx); err != nil {
		t.Fatalf("--push=false must not error, it just cannot publish: %v", err)
	}
	if got := originTip(t, dep, "main"); got != pushed {
		t.Error("--push=false must not publish anything")
	}
	if r.Tip != pushed {
		t.Errorf("Tip = %s, want origin tip %s (unpushed work excluded)", short(r.Tip), short(pushed))
	}
	if !strings.Contains(buf.String(), "excluding them") {
		t.Errorf("expected a note that unpushed work is excluded, got:\n%s", buf.String())
	}
}

// TestPushUnpushedDryRunPreviews: a dry run reports the intended push without doing it.
func TestPushUnpushedDryRunPreviews(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	dep := filepath.Join(base, "dep")
	ctx := context.Background()

	newCheckout(t, dep, "main")
	githelp(t, dep, "init", "--quiet", "--bare", "-b", "main", origin)
	githelp(t, dep, "remote", "add", "origin", origin)
	githelp(t, dep, "push", "-q", "-u", "origin", "main")
	pushed := originTip(t, dep, "main")
	githelp(t, dep, "commit", "-q", "--allow-empty", "-m", "local-only")

	var buf strings.Builder
	cfg := Defaults()
	cfg.Namespace = base
	cfg.Sync = false
	cfg.Push = true
	cfg.DryRun = true
	r := &Repo{Name: "dep", Path: dep, IsRepo: true}
	j := NewJournal(&buf)
	p := NewPipeline(cfg, &Scan{Repos: map[string]*Repo{"dep": r}, Order: []string{"dep"}}, j)

	if err := p.readState(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := p.pushUnpushed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := originTip(t, dep, "main"); got != pushed {
		t.Error("dry run must not push")
	}
	if !strings.Contains(buf.String(), "(dry run) would push") {
		t.Errorf("expected a dry-run preview line, got:\n%s", buf.String())
	}
}

// TestUnpushedOnFeatureBranchIsIgnored: unpushed work is measured on the release
// branch, not HEAD. A checked-out feature branch that is ahead must not be counted,
// pushed, or pinned -- otherwise feature work leaks onto the release branch.
func TestUnpushedOnFeatureBranchIsIgnored(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	dep := filepath.Join(base, "dep")
	ctx := context.Background()

	newCheckout(t, dep, "main")
	githelp(t, dep, "init", "--quiet", "--bare", "-b", "main", origin)
	githelp(t, dep, "remote", "add", "origin", origin)
	githelp(t, dep, "push", "-q", "-u", "origin", "main")
	pushed := originTip(t, dep, "main")

	// Unpushed work lives on a feature branch; main is clean and up to date.
	githelp(t, dep, "checkout", "-q", "-b", "feature")
	githelp(t, dep, "commit", "-q", "--allow-empty", "-m", "feature-only")
	githelp(t, dep, "checkout", "-q", "main")

	cfg := Defaults()
	cfg.Namespace = base
	cfg.Sync = false
	cfg.Push = true
	r := &Repo{Name: "dep", Path: dep, IsRepo: true}
	j := NewJournal(io.Discard)
	p := NewPipeline(cfg, &Scan{Repos: map[string]*Repo{"dep": r}, Order: []string{"dep"}}, j)

	if err := p.readState(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r.Ahead != 0 {
		t.Errorf("Ahead = %d, want 0: feature-branch commits must not count against the release branch", r.Ahead)
	}
	if err := p.pushUnpushed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := originTip(t, dep, "main"); got != pushed {
		t.Errorf("origin/main moved to %s: feature work must not be pushed onto the release branch", short(got))
	}
}
