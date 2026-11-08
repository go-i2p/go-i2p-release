package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// The tests here are regressions for defects found auditing the cobra migration.
// Each one is named for the behaviour that was broken.

// githelp runs git in dir and fails the test when it errors.
func githelp(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(bytes.TrimSpace(out))
}

// newCheckout makes a git repository with one empty commit on branch.
func newCheckout(t *testing.T, dir, branch string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	githelp(t, dir, "init", "--quiet", ".")
	githelp(t, dir, "commit", "-q", "--allow-empty", "-m", "seed")
	githelp(t, dir, "branch", "-M", branch)
}

// TestBareCommandAcceptsNoVersionFlag: cobra installs a boolean --version on the
// root command by itself. Binding it to viper overwrote the release `version` key
// with the string "false", so the default tip mode aborted with
// `"vfalse" is not a valid semantic version`.
func TestBareCommandDoesNotBindCobraVersionFlag(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	// Cobra adds its own boolean --version flag lazily, while executing, so the
	// regression has to be driven through Execute() rather than by binding the flag
	// set directly. The bare command resolves config, then discovers the namespace;
	// a poisoned `version` key fails at the point it would reach normalize().
	var got *Config
	var capture bool
	root := newRootCmd("test")
	ns := t.TempDir()
	root.SetArgs([]string{"--namespace", ns, "--dry-run"})
	// Replace the tip RunE with one that only captures the resolved config.
	root.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := resolveConfig(cmd)
		got = cfg
		capture = true
		return err
	}
	err := root.Execute()
	if !capture {
		t.Fatal("bare command did not run")
	}
	if err != nil {
		t.Fatalf("bare command: %v", err)
	}
	if got.Version != "" {
		t.Errorf("Version = %q, want empty for a tip run", got.Version)
	}
}

// TestDefaultModeIsTipCheckIn pins the mode the user asked for as the default:
// update to tips, verify, commit and push, without tagging.
func TestDefaultModeIsTipCheckIn(t *testing.T) {
	d := Defaults()
	if d.DryRun {
		t.Error("default must not be a dry run; the default mode performs the check-in")
	}
	if !d.Push {
		t.Error("default must push, so a tip check-in actually lands")
	}
	if d.Verify != verifyTest {
		t.Errorf("default verify = %q, want %q", d.Verify, verifyTest)
	}
	if d.Version != "" {
		t.Error("default must not carry a version; tagging is what release is for")
	}
}

// TestReleaseVersionFlagStillBinds keeps the fix above from disabling the
// release subcommand's own --version, which is a string flag. It drives the whole
// command pipeline so the subcommand's flag set is the one viper binds.
func TestReleaseVersionFlagStillBinds(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	var got *Config
	root := newRootCmd("test")
	for _, c := range root.Commands() {
		if c.Name() == "release" {
			c.RunE = func(cmd *cobra.Command, args []string) error {
				cfg, err := resolveConfig(cmd)
				got = cfg
				return err
			}
		}
	}
	root.SetArgs([]string{"release", "--version", "0.2.0", "--namespace", t.TempDir()})
	if err := root.Execute(); err != nil {
		t.Fatalf("release --version 0.2.0: %v", err)
	}
	if got == nil {
		t.Fatal("release command did not run")
	}
	if got.Version != "v0.2.0" {
		t.Errorf("Version = %q, want v0.2.0", got.Version)
	}
}

// TestPreflightRejectsMajorVersionRegardlessOfPolicy: a v2+ tag cannot be imported
// without a /v2 module path, which this namespace does not use. That is fatal even
// when --tag-existing is skip or recreate.
func TestPreflightRejectsMajorVersionRegardlessOfPolicy(t *testing.T) {
	for _, policy := range []string{tagFail, tagSkip, tagRecreate} {
		cfg := Defaults()
		cfg.Version = "v2.0.0"
		cfg.TagExisting = policy
		p := NewPipeline(cfg, &Scan{Repos: map[string]*Repo{}, Order: nil}, NewJournal(io.Discard))
		err := p.preflightTags(context.Background())
		if err == nil {
			t.Errorf("--tag-existing=%s: v2.0.0 must be refused", policy)
			continue
		}
		if !strings.Contains(err.Error(), "/v2") {
			t.Errorf("--tag-existing=%s: unexpected error %v", policy, err)
		}
	}
}

// TestTagLocationsFindsRemoteOnlyTag: `git fetch` does not create
// refs/remotes/origin/<tag> for tags, so probing that ref misses a tag that exists
// only on the host -- and --tag-existing=fail would re-create and force-push over
// it. A --no-tags clone is the deterministic case where the tag is on origin but
// invisible to every local ref.
func TestTagLocationsFindsRemoteOnlyTag(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	seed := filepath.Join(base, "seed")
	newCheckout(t, seed, "main")
	githelp(t, seed, "init", "--quiet", "--bare", origin)
	githelp(t, seed, "remote", "add", "origin", origin)
	githelp(t, seed, "push", "-q", "origin", "main")
	githelp(t, seed, "tag", "v0.3.3")
	githelp(t, seed, "push", "-q", "origin", "refs/tags/v0.3.3")

	work := filepath.Join(base, "work")
	githelp(t, base, "clone", "-q", "--no-tags", origin, "work")

	ctx := context.Background()
	cfg := Defaults()
	cfg.Version = "v0.3.3"
	j := NewJournal(io.Discard)
	r := &Repo{Name: "work", Path: work, IsRepo: true}
	p := NewPipeline(cfg, &Scan{Repos: map[string]*Repo{"work": r}, Order: []string{"work"}}, j)

	// The probe the original code used sees nothing here: that is the bug.
	if out, _ := p.exec.Quiet(ctx, work, "git", "rev-parse", "--verify", "refs/remotes/origin/v0.3.3"); out != "" {
		t.Skip("remote-tracking ref exists for the tag; this git behaves differently")
	}
	local, remote := p.tagLocations(ctx, r)
	if local != "" {
		t.Errorf("local tag should be absent, got %q", local)
	}
	if remote == "" {
		t.Error("tagLocations must see the tag that exists on origin")
	}

	// And preflightTags must refuse to release over it.
	if err := p.preflightTags(ctx); err == nil {
		t.Error("preflightTags must fail when the tag exists only on origin")
	}
}

// TestSnapshotRestoreRoundTrip: a failed bump has to leave go.mod/go.sum exactly as
// it found them, including a local `replace` that the bump dropped.
func TestSnapshotRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	goMod := "module example.com/x\n\ngo 1.21\n\nrequire example.com/y v0.1.0\n\nreplace example.com/y => ../y\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := snapshotFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.HadFile["go.mod"] || snap.HadFile["go.sum"] {
		t.Errorf("HadFile = %v, want go.mod only", snap.HadFile)
	}

	// Rewrite go.mod and create a go.sum, as a bump would.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	touched := snap.restore(dir)
	if len(touched) != 2 {
		t.Errorf("restore touched %v, want go.mod and go.sum", touched)
	}
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil || string(data) != goMod {
		t.Errorf("go.mod not restored: %q (err %v)", string(data), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); !os.IsNotExist(err) {
		t.Error("a go.sum the snapshot did not record should be removed")
	}
}

// TestChangedModFilesIgnoresMissingSum: `git add go.mod go.sum` fails atomically
// when go.sum is absent, which left go.mod unstaged and let the following bare
// `git commit` ship whatever the user had staged for something else.
func TestChangedModFilesIgnoresMissingSum(t *testing.T) {
	cases := []struct {
		status string
		want   []string
	}{
		{" M go.mod", []string{"go.mod"}},
		{" M go.mod\n M go.sum", []string{"go.mod", "go.sum"}},
		{"?? go.sum", []string{"go.sum"}},
		{"A  go.mod\nD  go.sum", []string{"go.mod", "go.sum"}},
		{"", nil},
		{" M unrelated.go", nil},
		{"R  go.mod.bak -> go.mod", []string{"go.mod"}},
	}
	for _, tc := range cases {
		got := changedModFiles(tc.status)
		if len(got) != len(tc.want) {
			t.Errorf("changedModFiles(%q) = %v, want %v", tc.status, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("changedModFiles(%q) = %v, want %v", tc.status, got, tc.want)
				break
			}
		}
	}
}

// TestChainErrorUnwrapsCancellation: without Unwrap, a Ctrl-C that arrived after any
// command had already failed was reported as a generic error and exited 1.
func TestChainErrorUnwrapsCancellation(t *testing.T) {
	ce := &chainError{msg: "boom", causes: []error{errors.New("git add: exit status 1"), context.Canceled}}
	if !errors.Is(ce, context.Canceled) {
		t.Fatal("chainError must unwrap so errors.Is can see context.Canceled")
	}
}

// TestLibsExcludesClientNamedByOnly: --only used to bypass the layer filters, so
// `--libs --only i2ptui` silently released an application.
func TestLibsExcludesClientNamedByOnly(t *testing.T) {
	got := selectNames(&Config{Libs: true, Only: []string{"logger", "i2ptui"}}, KnownOrder)
	if len(got) != 1 || got[0] != "logger" {
		t.Errorf("--libs --only logger,i2ptui = %v, want [logger]", got)
	}

	// The inverse must hold too.
	got = selectNames(&Config{Clients: true, Only: []string{"logger", "i2ptui"}}, KnownOrder)
	if len(got) != 1 || got[0] != "i2ptui" {
		t.Errorf("--clients --only logger,i2ptui = %v, want [i2ptui]", got)
	}
}

// TestLayerFilterDropsUnknownWithWarning: a module with no known layer cannot be
// classified, so a layer filter drops it and says why rather than assuming.
func TestLayerFilterDropsUnknownWithWarning(t *testing.T) {
	candidates := []string{"newmod", "logger"}
	cfg := &Config{Libs: true, All: true}
	got := selectNames(cfg, candidates)
	if len(got) != 1 || got[0] != "logger" {
		t.Errorf("--libs with an unknown module = %v, want [logger]", got)
	}
	var warned bool
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "newmod") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a warning naming newmod, got %v", cfg.Warnings())
	}

	// Without a layer filter the unknown module is still selectable.
	got = selectNames(&Config{All: true}, candidates)
	if !contains(got, "newmod") {
		t.Errorf("--all should keep an unknown module, got %v", got)
	}
}

// TestPlanHonoursQuiet: plan's table is the command's deliverable and must survive
// --quiet, while the surrounding commentary must not.
func TestPlanHonoursQuiet(t *testing.T) {
	cfg := Defaults()
	cfg.Quiet = true
	cfg.Version = "v0.1.8"
	cfg.Push = false
	scan := &Scan{
		Repos: map[string]*Repo{"logger": {Name: "logger", Branch: "main", Tip: "0123456789abcdef"}},
		Order: []string{"logger"},
	}
	var buf bytes.Buffer
	j := NewJournal(&buf)
	j.SetQuiet(true)
	p := NewPipeline(cfg, scan, j)
	p.plan()

	out := buf.String()
	if !strings.Contains(out, "logger") || !strings.Contains(out, "REPOSITORY") {
		t.Errorf("--quiet must still print the plan table, got %q", out)
	}
	if strings.Contains(out, "Release plan") {
		t.Errorf("--quiet should suppress section headings, got %q", out)
	}

	verbose := NewJournal(io.Discard)
	p2 := NewPipeline(Defaults(), scan, verbose)
	p2.plan()
	if len(verbose.Actions) != 0 {
		t.Error("plan must not record journal actions")
	}
}

// TestNonGitModuleSkippedNotFatal: --all walks every directory under the namespace,
// and one that holds a go.mod but no git metadata used to abort the whole run.
func TestNonGitModuleSkippedNotFatal(t *testing.T) {
	base := t.TempDir()
	// A real checkout from the known set.
	repo := filepath.Join(base, "logger")
	newCheckout(t, repo, "main")
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module github.com/go-i2p/logger\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stray directory that is not a repository.
	stray := filepath.Join(base, "not-a-checkout")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "go.mod"), []byte("module github.com/go-i2p/not-a-checkout\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Defaults()
	cfg.Namespace = base
	cfg.All = true
	scan, err := Discover(cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	r := scan.Repos["not-a-checkout"]
	if r == nil {
		t.Fatal("stray module missing from the scan")
	}
	if r.SkipReason == "" {
		t.Error("stray module should carry a skip reason")
	}
	if len(scan.workOrder()) != 1 || scan.workOrder()[0] != "logger" {
		t.Errorf("work order should exclude the skipped module, got %v", scan.workOrder())
	}

	// A known repository that is not a checkout is a hard error: silently dropping a
	// core module from a namespace-wide release is worse than failing.
	kcfg := Defaults()
	kcfg.Namespace = base
	missing := filepath.Join(base, "common")
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(missing, "go.mod"), []byte("module github.com/go-i2p/common\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kcfg.Only = []string{"common"}
	if _, err := Discover(kcfg); err == nil {
		t.Error("a known release-set directory with no .git must fail rather than be skipped")
	}
}
