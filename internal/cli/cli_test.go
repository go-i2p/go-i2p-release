package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTopoSortOrdersDepsFirst(t *testing.T) {
	scan := &Scan{Repos: map[string]*Repo{
		"go-i2p": {Name: "go-i2p", Deps: []string{"common", "crypto", "logger"}},
		"common": {Name: "common", Deps: []string{"crypto", "logger"}},
		"crypto": {Name: "crypto", Deps: []string{"logger"}},
		"logger": {Name: "logger"},
		"i2ptui": {Name: "i2ptui"},
	}}
	scan.Order = topoSort(scan)

	pos := map[string]int{}
	for i, n := range scan.Order {
		pos[n] = i
	}
	if len(scan.Order) != len(scan.Repos) {
		t.Fatalf("expected %d entries, got %d: %v", len(scan.Repos), len(scan.Order), scan.Order)
	}
	for name, r := range scan.Repos {
		for _, d := range r.Deps {
			if pos[d] >= pos[name] {
				t.Errorf("%s (position %d) must come after dependency %s (position %d); order = %v",
					name, pos[name], d, pos[d], scan.Order)
			}
		}
	}
	if pos["logger"] != 0 {
		t.Errorf("expected logger first in layer order, order = %v", scan.Order)
	}
}

func TestTopoSortDetectsCycle(t *testing.T) {
	scan := &Scan{Repos: map[string]*Repo{
		"a": {Name: "a", Deps: []string{"b"}},
		"b": {Name: "b", Deps: []string{"a"}},
	}}
	scan.Order = topoSort(scan)
	if len(scan.Order) != 2 {
		t.Fatalf("cycle must still emit every repo, got %v", scan.Order)
	}
	var found bool
	for _, w := range scan.Warnings {
		if strings.Contains(w, "cycle") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a cycle warning, got %v", scan.Warnings)
	}
}

func TestSelectNamesLibsAndClients(t *testing.T) {
	candidates := KnownOrder
	libs := selectNames(&Config{Libs: true}, candidates)
	clients := selectNames(&Config{Clients: true}, candidates)

	for _, n := range clients {
		if layerOf(n) < ClientLayer {
			t.Errorf("--clients returned library repo %s from layer %d", n, layerOf(n))
		}
	}
	if len(clients) == 0 {
		t.Error("--clients returned nothing")
	}
	for _, n := range libs {
		if layerOf(n) >= ClientLayer {
			t.Errorf("--libs returned client-layer repo %s", n)
		}
	}
	if len(libs) != len(KnownOrder)-len(clients) {
		t.Errorf("--libs and --clients should partition the known set: %d + %d != %d",
			len(libs), len(clients), len(KnownOrder))
	}
}

func TestSelectNamesOnlyAndExclude(t *testing.T) {
	got := selectNames(&Config{Only: []string{"logger", "common"}, Exclude: []string{"common"}}, KnownOrder)
	if len(got) != 1 || got[0] != "logger" {
		t.Errorf("--only/--exclude not applied: %v", got)
	}
}

func TestNormalizeVersion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"v0.1.7", "v0.1.7"},
		{"0.1.7", "v0.1.7"},
		{"v1.0.0-rc1", "v1.0.0-rc1"},
	} {
		got, err := NormalizeVersion(tc.in)
		if err != nil {
			t.Errorf("NormalizeVersion(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "not-a-version", "v1.2.3.4"} {
		if _, err := NormalizeVersion(bad); err == nil {
			t.Errorf("NormalizeVersion(%q) should fail", bad)
		}
	}
}

func TestReadModuleFindsPeersAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/go.mod"
	if err := os.WriteFile(path, []byte(`module github.com/go-i2p/example

go 1.21

require (
	github.com/go-i2p/logger v0.1.0
	github.com/go-i2p/common v0.1.0
	github.com/stretchr/testify v1.9.0
)

replace github.com/go-i2p/logger => ../logger
`), 0o644); err != nil {
		t.Fatal(err)
	}
	module, deps, warns := readModule(path, DefaultPrefix)
	if module != "github.com/go-i2p/example" {
		t.Errorf("module = %q", module)
	}
	if strings.Join(deps, ",") != "logger,common" {
		t.Errorf("deps = %v, want the two namespace peers", deps)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "local replace") {
		t.Errorf("warns = %v, want the local replace flagged", warns)
	}
}

// TestQuietIgnoresStderr guards a real bug: probing for a nonexistent tag with
// `git rev-parse --verify` writes "fatal:" to stderr, which must not be returned as
// output or the caller concludes the tag exists.
func TestQuietIgnoresStderr(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	cfg := Defaults()
	cfg.Namespace = dir
	j := NewJournal(io.Discard)
	ex := NewExecutor(cfg, j)

	if _, err := ex.Quiet(ctx, dir, "git", "init", "--quiet", "."); err != nil {
		t.Skipf("git init failed: %v", err)
	}
	out, err := ex.Quiet(ctx, dir, "git", "rev-parse", "--verify", "refs/tags/v1.0.0")
	if err == nil {
		t.Fatal("expected rev-parse to fail for a missing tag")
	}
	if out != "" {
		t.Errorf("Quiet returned %q for a failed command; want empty output", out)
	}

	// A tag that does exist must come back as its hash.
	if _, err := ex.Quiet(ctx, dir, "git", "commit", "-q", "--allow-empty", "-m", "seed"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	if _, err := ex.Quiet(ctx, dir, "git", "tag", "v1.0.0"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	out, err = ex.Quiet(ctx, dir, "git", "rev-parse", "--verify", "refs/tags/v1.0.0")
	if err != nil || len(out) < 40 {
		t.Errorf("existing tag: out=%q err=%v", out, err)
	}
}

// TestStaleOriginHeadIsIgnored guards the hazard that caused a bad push: a local
// origin/HEAD naming a branch that is not on the remote. Selecting it would create a
// new branch on the remote from the wrong work.
func TestStaleOriginHeadIsIgnored(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	cfg := Defaults()
	cfg.Namespace = dir
	j := NewJournal(io.Discard)
	ex := NewExecutor(cfg, j)
	q := func(args ...string) {
		if _, err := ex.Quiet(ctx, dir, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	q("git", "init", "--quiet", ".")
	q("git", "config", "user.email", "t@example.com")
	q("git", "config", "user.name", "t")
	q("git", "commit", "-q", "--allow-empty", "-m", "seed")
	q("git", "branch", "-M", "main")
	// A bare "origin" that only carries main.
	q("git", "init", "--quiet", "--bare", "../origin.git")
	q("git", "remote", "add", "origin", "../origin.git")
	q("git", "push", "-q", "origin", "main")
	q("git", "fetch", "-q", "origin")
	// Point origin/HEAD at a branch that does not exist on origin.
	q("git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")

	p := NewPipeline(cfg, &Scan{Repos: map[string]*Repo{"x": {Name: "x", Path: dir}}, Order: []string{"x"}}, j)
	branch, _ := p.detectBranch(ctx, p.scan.Repos["x"])
	if branch != "main" {
		t.Errorf("detectBranch() = %q, want main (stale origin/HEAD must be ignored)", branch)
	}
	if p.scan.Repos["x"].RemoteBranch && branch == "master" {
		t.Error("must not treat a branch absent from origin as usable")
	}
}

func TestDefaultsNamespaceUsesGopath(t *testing.T) {
	t.Setenv("GOPATH", "/custom/gopath")
	ns, err := DefaultNamespace()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/custom/gopath", "src", DefaultPrefix)
	if ns != want {
		t.Errorf("DefaultNamespace() = %q, want %q", ns, want)
	}
}

func TestConfigEnvStandaloneAndPrivate(t *testing.T) {
	cfg := &Config{Prefix: DefaultPrefix, Standalone: true, Private: true}
	env := strings.Join(cfg.Env(), "\n")
	for _, want := range []string{"GOWORK=off", "GOFLAGS=-mod=mod", "GOPRIVATE=" + DefaultPrefix + "/*"} {
		if !strings.Contains(env, want) {
			t.Errorf("Env() missing %q", want)
		}
	}
	off := &Config{Prefix: DefaultPrefix}
	if strings.Contains(strings.Join(off.Env(), "\n"), "GOWORK=off") {
		t.Error("GOWORK should be untouched when --standalone is false")
	}
}
