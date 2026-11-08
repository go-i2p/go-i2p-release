package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// DefaultPrefix is the module namespace this tool releases by default.
const DefaultPrefix = "github.com/go-i2p"

// DefaultMessage is the commit message used for dependency check-ins.
const DefaultMessage = "Update go-i2p dependencies to branch tips"

// Layers orders the known go-i2p repositories so that dependencies precede
// dependents. It is the default release set and the tie-breaker for the
// topological sort. Repositories not listed here are only picked up with --all.
var Layers = [][]string{
	// Layer 0: core libraries, no internal dependencies.
	{"logger", "elgamal", "su3", "crypto"},
	// Layer 1: shared structures.
	{"common"},
	// Layer 2: network layer.
	{"noise", "go-nat-listener", "path", "pool", "go-noise"},
	// Layer 3: router and protocol clients.
	{"go-i2p", "go-i2cp"},
	// Layer 4: applications and tunnels.
	{"go-datagrams", "go-streaming", "go-sam-bridge", "i2p-control", "i2ptui"},
}

// ClientLayer is the first index in Layers that holds executables rather than
// reusable libraries: the router, protocol clients, tunnels and tools. --libs
// keeps the layers below it; --clients keeps it and everything above.
const ClientLayer = 3

// KnownOrder is Layers flattened.
var KnownOrder = func() []string {
	var out []string
	for _, l := range Layers {
		out = append(out, l...)
	}
	return out
}()

// layerOf returns the index of a repository in Layers, or -1 when unknown.
func layerOf(name string) int {
	for i, layer := range Layers {
		for _, n := range layer {
			if n == name {
				return i
			}
		}
	}
	return -1
}

// Repo is a module inside the namespace plus the state observed on disk.
type Repo struct {
	Name   string
	Path   string
	Module string
	// Deps are namespace peers this module requires, by repository name.
	Deps []string

	// IsRepo is false when the directory holds a go.mod but no git metadata. Such
	// modules are listed by the plan but dropped from the mutating steps.
	IsRepo bool
	// SkipReason explains why a repository was excluded from the mutating steps.
	SkipReason string

	// Detected while inspecting.
	Branch  string // default branch of the repository
	Current string // branch actually checked out
	Head    string // commit checked out locally
	Tip     string // head of the branch on origin, after fetch
	Dirty   bool
	// DirtyFiles lists tracked changes that made the repository dirty.
	DirtyFiles []string
	// Untracked lists untracked paths, which are reported but do not block a run.
	Untracked []string
	// HasPendingCommit records whether this run created (or would create) a
	// dependency commit, so publishing skips repositories with nothing to push.
	HasPendingCommit bool
	// RemoteBranch confirms that refs/remotes/origin/<Branch> actually exists. A
	// stale origin/HEAD can name a branch that is not on the remote at all, and
	// pushing to it would silently create one.
	RemoteBranch bool

	// Snapshot holds the pre-bump go.mod/go.sum so a failed run can be unwound.
	// Set by the bump steps before they touch the working tree.
	Snapshot *modSnapshot

	// remotes caches the branch names seen on origin so detection is one ls-remote;
	// branchDetected stops the choice (and its warnings) being recomputed.
	remotes        map[string]bool
	branchDetected bool
	Warnings       []string
}

// Scan is the discovered release set.
type Scan struct {
	Repos    map[string]*Repo
	Order    []string
	Prefix   string
	Warnings []string
}

// Discover builds the release set for cfg and reads each module's requirements.
func Discover(cfg *Config) (*Scan, error) {
	candidates, err := listModules(cfg)
	if err != nil {
		return nil, err
	}
	sel := selectNames(cfg, candidates)
	if len(sel) == 0 {
		return nil, fmt.Errorf("no repositories selected in %s (check --only/--exclude/--libs/--clients/--all)", cfg.Namespace)
	}

	scan := &Scan{Repos: map[string]*Repo{}, Prefix: cfg.Prefix}
	for _, name := range sel {
		path := filepath.Join(cfg.Namespace, name)
		r := &Repo{Name: name, Path: path, IsRepo: hasGitMetadata(path)}
		module, deps, warns := readModule(filepath.Join(path, "go.mod"), cfg.Prefix)
		r.Module, r.Deps, r.Warnings = module, deps, warns
		if !r.IsRepo {
			// A repository from the known release set that is not a checkout is a
			// real problem, not something to walk past: it would silently leave a
			// core module out of a namespace-wide release.
			if layerOf(name) != -1 {
				return nil, fmt.Errorf("%s: %s is in the release set but is not a git repository (%s has no .git); clone it or exclude it with --exclude",
					name, path, path)
			}
			r.SkipReason = "not a git repository"
		}
		if module != "" && module != cfg.Prefix+"/"+name {
			r.Warnings = append(r.Warnings, fmt.Sprintf("module path %q does not match %s/%s; using the path declared in go.mod",
				module, cfg.Prefix, name))
		}
		scan.Repos[name] = r
	}

	// Keep only dependency edges that land inside the selected set.
	for _, r := range scan.Repos {
		var kept []string
		for _, d := range r.Deps {
			if _, ok := scan.Repos[d]; ok && d != r.Name && !contains(kept, d) {
				kept = append(kept, d)
			}
		}
		r.Deps = kept
	}

	scan.Order = topoSort(scan)
	scan.Warnings = append(cfg.Warnings(), scan.Warnings...)
	return scan, nil
}

// listModules returns the repository names eligible for selection.
func listModules(cfg *Config) ([]string, error) {
	if !cfg.All {
		if len(cfg.Only) > 0 {
			// The user named the repositories explicitly; only those need to exist.
			var out []string
			for _, name := range cfg.Only {
				if hasGoMod(filepath.Join(cfg.Namespace, name)) {
					out = append(out, name)
				}
			}
			return out, nil
		}
		var out []string
		for _, name := range KnownOrder {
			if hasGoMod(filepath.Join(cfg.Namespace, name)) {
				out = append(out, name)
				continue
			}
			cfg.Warnf("known repo %s has no go.mod under %s; skipping", name, filepath.Join(cfg.Namespace, name))
		}
		return out, nil
	}

	entries, err := os.ReadDir(cfg.Namespace)
	if err != nil {
		return nil, fmt.Errorf("reading namespace %s: %w", cfg.Namespace, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(cfg.Namespace, name)
		info, err := os.Stat(path) // follows symlinks, common in GOPATH layouts
		if err != nil || !info.IsDir() || !hasGoMod(path) {
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func hasGoMod(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
}

// hasGitMetadata reports whether dir looks like a git checkout. It is a cheap
// filesystem probe so discovery can flag non-repository modules before any git
// command runs.
func hasGitMetadata(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// selectNames applies --only/--exclude/--libs/--clients to the candidate list.
func selectNames(cfg *Config, candidates []string) []string {
	available := map[string]bool{}
	for _, c := range candidates {
		available[c] = true
	}

	keep := func(name string) bool {
		if !available[name] {
			return false
		}
		if len(cfg.Only) > 0 && !contains(cfg.Only, name) {
			return false
		}
		if contains(cfg.Exclude, name) {
			return false
		}
		layer := layerOf(name)
		// --libs/--clients are applied to every candidate, including one named
		// explicitly by --only, so `--libs --only i2ptui` cannot quietly release an
		// application. A module outside the known layers has no verifiable layer, so
		// a layer filter drops it with a warning rather than assuming it qualifies.
		if cfg.Libs || cfg.Clients {
			if layer == -1 {
				cfg.Warnf("%s is not in the known layer list, so --libs/--clients cannot classify it; excluded", name)
				return false
			}
			if cfg.Libs && layer >= ClientLayer {
				return false
			}
			if cfg.Clients && layer < ClientLayer {
				return false
			}
		}
		return true
	}

	var out []string
	for _, name := range KnownOrder {
		if keep(name) {
			out = append(out, name)
		}
	}
	// Modules outside the known set are appended alphabetically. Without --all they
	// only reach here when named explicitly by --only.
	var extra []string
	for _, name := range candidates {
		if layerOf(name) == -1 && keep(name) {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	out = append(out, extra...)
	for _, name := range cfg.Only {
		if !available[name] {
			cfg.Warnf("--only %s is not a module under %s; ignored", name, cfg.Namespace)
		}
	}
	for _, name := range cfg.Exclude {
		if !available[name] {
			cfg.Warnf("--exclude %s is not in the release set; ignored", name)
		}
	}
	return out
}

// readModule parses go.mod and returns its module path plus the namespace peers it requires.
func readModule(path, prefix string) (string, []string, []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, []string{fmt.Sprintf("reading %s: %v", path, err)}
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return "", nil, []string{fmt.Sprintf("parsing %s: %v", path, err)}
	}
	var deps, warns []string
	for _, req := range f.Require {
		if !strings.HasPrefix(req.Mod.Path, prefix+"/") {
			continue
		}
		name := strings.TrimPrefix(req.Mod.Path, prefix+"/")
		if name != "" {
			deps = append(deps, name)
		}
	}
	// replace directives inside the namespace must be removed before a release, or
	// the published module graph will not match what was tagged.
	for _, rep := range f.Replace {
		if rep == nil {
			continue
		}
		// modfile models "replace X => ./dir" as New.Path holding the directory with an
		// empty version; "replace X => Y v1.2.3" sets both.
		if !strings.HasPrefix(rep.Old.Path, prefix+"/") {
			continue
		}
		if rep.New.Version == "" {
			warns = append(warns, fmt.Sprintf("local replace %s => %s will be dropped before bumping",
				rep.Old.Path, rep.New.Path))
		} else if strings.HasPrefix(rep.New.Path, prefix+"/") {
			warns = append(warns, fmt.Sprintf("replace %s => %s %s left in place; verify it is intentional",
				rep.Old.Path, rep.New.Path, rep.New.Version))
		}
	}
	if f.Module == nil {
		return "", deps, append(warns, fmt.Sprintf("%s has no module directive", path))
	}
	return f.Module.Mod.Path, deps, warns
}

// topoSort orders repos so every module precedes its dependents, using the known
// layer order as the tie-breaker. A cycle falls back to layer order with a warning.
func topoSort(scan *Scan) []string {
	names := make([]string, 0, len(scan.Repos))
	for n := range scan.Repos {
		names = append(names, n)
	}
	sort.SliceStable(names, func(i, j int) bool { return lessLayer(names[i], names[j]) })

	pending := map[string]int{}
	for _, n := range names {
		pending[n] = len(scan.Repos[n].Deps)
	}

	var out []string
	remaining := names
	for len(remaining) > 0 {
		var ready []string
		for _, n := range remaining {
			if pending[n] == 0 {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 {
			scan.Warnings = append(scan.Warnings,
				fmt.Sprintf("dependency cycle among %s; falling back to layer order", strings.Join(remaining, ", ")))
			out = append(out, remaining...)
			break
		}
		emitted := map[string]bool{}
		for _, n := range ready {
			out = append(out, n)
			emitted[n] = true
			pending[n] = -1
		}
		for _, m := range remaining {
			if pending[m] < 0 || emitted[m] {
				continue
			}
			for _, d := range scan.Repos[m].Deps {
				if emitted[d] {
					pending[m]--
				}
			}
		}
		var left []string
		for _, n := range remaining {
			if pending[n] >= 0 {
				left = append(left, n)
			}
		}
		remaining = left
	}
	return out
}

func lessLayer(a, b string) bool {
	la, lb := layerOf(a), layerOf(b)
	if la != lb {
		if la == -1 {
			return false
		}
		if lb == -1 {
			return true
		}
		return la < lb
	}
	return a < b
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// splitLines splits command output into non-empty lines.
func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}
