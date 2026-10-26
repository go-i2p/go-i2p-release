package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repository configuration
type Repo struct {
	Name  string
	Path  string
	Order int
}

// The complete release order for go-i2p repositories
var releaseOrder = []string{
	// Layer 1: Core libraries (no internal dependencies)
	"logger",
	"elgamal",
	"su3",
	"crypto",
	// Layer 2: Common structures
	"common",
	// Layer 3: Network layer
	"noise",
	"go-nat-listener",
	"path",
	"pool",
	"go-noise",
	// Layer 4: Router and clients
	"go-i2p",
	"go-i2cp",
	// Layer 5: Application layer
	"go-datagrams",
	"go-streaming",
	"go-sam-bridge",
	"i2p-control",
	"i2ptui",
}

func run(cmd string, args []string, dir string) error {
	c := exec.Command(cmd, args...)
	c.Dir = dir
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func runOutput(cmd string, args []string, dir string) (string, error) {
	c := exec.Command(cmd, args...)
	c.Dir = dir
	out, err := c.Output()
	return strings.TrimSpace(string(out)), err
}

func main() {
	version := os.Getenv("VERSION")
	if version == "" {
		if len(os.Args) < 2 {
			fmt.Println("Usage: go-i2p-release <version> [go-i2p-dir]")
			fmt.Println("  or:  VERSION=v0.1.7 go-i2p-release")
			os.Exit(1)
		}
		version = os.Args[1]
	}

	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	baseDir := "."
	if len(os.Args) > 2 {
		baseDir = os.Args[2]
	}

	dryRun := os.Getenv("DRY_RUN") == "true"
	libsOnly := os.Getenv("LIBS") == "true"

	if dryRun {
		fmt.Println("DRY_RUN=true: Will not tag or push")
	}
	if libsOnly {
		fmt.Println("LIBS=true: Will only release libraries, not clients")
		// Filter to only library repos
		libs := []string{"logger", "elgamal", "su3", "crypto", "common", "noise", "go-nat-listener", "path", "pool", "go-noise"}
		releaseOrder = libs
	}

	// Collect repository paths
	repos := make(map[string]Repo)
	for i, name := range releaseOrder {
		repoPath := filepath.Join(baseDir, name)
		if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err != nil {
			fmt.Printf("Warning: go.mod not found in %s: %v\n", repoPath, err)
			continue
		}
		repos[name] = Repo{Name: name, Path: repoPath, Order: i}
	}

	fmt.Printf("Found %d repositories to release\n", len(repos))

	// Step 1: Sync all repos to latest
	fmt.Println("\n=== Step 1: Syncing all repos to latest ===")
	for name, repo := range repos {
		fmt.Printf("  Syncing %s...\n", name)
		if err := run("git", []string{"fetch", "origin"}, repo.Path); err != nil {
			fmt.Printf("    Error fetching %s: %v\n", name, err)
		}
		if err := run("git", []string{"checkout", "main"}, repo.Path); err != nil {
			fmt.Printf("    Error checking out %s: %v\n", name, err)
		}
		if err := run("git", []string{"pull", "--ff-only", "origin", "main"}, repo.Path); err != nil {
			fmt.Printf("    Error pulling %s: %v\n", name, err)
		}
	}

	// Step 2: Collect hashes for all repos
	fmt.Println("\n=== Step 2: Collecting commit hashes ===")
	hashes := make(map[string]string)
	for name, repo := range repos {
		hash, err := runOutput("git", []string{"rev-parse", "HEAD"}, repo.Path)
		if err != nil {
			fmt.Printf("  Error getting hash for %s: %v\n", name, err)
			continue
		}
		hashes[name] = hash
		fmt.Printf("  %s: %s\n", name, hash[:8])
	}

	// Step 3: Update go modules in each repo to point to latest of all deps
	fmt.Println("\n=== Step 3: Updating go module dependencies ===")
	for _, name := range releaseOrder {
		repo, ok := repos[name]
		if !ok {
			continue
		}

		fmt.Printf("  Updating %s dependencies...\n", name)

		// Update all go-i2p dependencies in this repo
		for _, dep := range releaseOrder {
			if dep == name {
				continue // Skip self
			}
			depRepo, ok := repos[dep]
			if !ok {
				continue
			}

			modulePath := "github.com/go-i2p/" + dep
			depHash := hashes[dep]

			// Drop any local replace directives for this dependency
			run("go", []string{"mod", "edit", "-dropreplace=" + modulePath}, depRepo.Path)

			// Use go get to update to the latest commit
			if err := run("go", []string{"get", modulePath + "@" + depHash, "-f"}, depRepo.Path); err != nil {
				fmt.Printf("    Error updating %s in %s: %v\n", dep, name, err)
			}
		}

		// Tidy and build to verify
		if err := run("go", []string{"mod", "tidy"}, repo.Path); err != nil {
			fmt.Printf("    Error running go mod tidy in %s: %v\n", name, err)
		}
	}

	// Step 4: Tag all repos with the version
	fmt.Println("\n=== Step 4: Tagging all repos ===")
	for _, name := range releaseOrder {
		repo, ok := repos[name]
		if !ok {
			continue
		}

		fmt.Printf("  Tagging %s with %s...\n", name, version)

		// Check if tag already exists
		exists, _ := runOutput("git", []string{"rev-parse", "refs/tags/" + version}, repo.Path)
		if exists != "" {
			// Delete old tag
			run("git", []string{"tag", "-d", version}, repo.Path)
		}

		// Create annotated tag
		if dryRun {
			fmt.Printf("    (dry run) Would tag %s with %s\n", name, version)
		} else {
			if err := run("git", []string{"tag", "-a", version, "-m", "Release " + version}, repo.Path); err != nil {
				fmt.Printf("    Error tagging %s: %v\n", name, err)
			}
		}
	}

	// Step 5: Push all tags and branches
	fmt.Println("\n=== Step 5: Pushing all repos ===")
	for _, name := range releaseOrder {
		repo, ok := repos[name]
		if !ok {
			continue
		}

		fmt.Printf("  Pushing %s...\n", name)

		if dryRun {
			fmt.Printf("    (dry run) Would push %s\n", name)
		} else {
			// Push main branch
			if err := run("git", []string{"push", "origin", "main"}, repo.Path); err != nil {
				fmt.Printf("    Error pushing main for %s: %v\n", name, err)
			}
			// Push tags
			if err := run("git", []string{"push", "origin", version}, repo.Path); err != nil {
				fmt.Printf("    Error pushing tag for %s: %v\n", name, err)
			}
		}
	}

	// Step 6: Update all repos to use the tagged version
	fmt.Println("\n=== Step 6: Updating to tagged version ===")
	for _, name := range releaseOrder {
		repo, ok := repos[name]
		if !ok {
			continue
		}

		fmt.Printf("  Updating %s to tagged %s...\n", name, version)

		for _, dep := range releaseOrder {
			if dep == name {
				continue
			}
			depRepo, ok := repos[dep]
			if !ok {
				continue
			}

			modulePath := "github.com/go-i2p/" + dep

			// Update to use the tagged version
			if err := run("go", []string{"get", modulePath + "@" + version, "-f"}, depRepo.Path); err != nil {
				fmt.Printf("    Error updating %s to %s: %v\n", dep, version, err)
			}
		}

		// Tidy again
		if err := run("go", []string{"mod", "tidy"}, repo.Path); err != nil {
			fmt.Printf("    Error tidying %s: %v\n", name, err)
		}

		// Commit the updated go.mod/go.sum
		if dryRun {
			fmt.Printf("    (dry run) Would commit updated deps for %s\n", name)
		} else {
			run("git", []string{"add", "go.mod", "go.sum"}, repo.Path)
			if err := run("git", []string{"commit", "-m", "Update dependencies to " + version, "--allow-empty"}, repo.Path); err != nil {
				fmt.Printf("    Error committing %s: %v\n", name, err)
			}
			// Push the updated commit
			if err := run("git", []string{"push", "origin", "main"}, repo.Path); err != nil {
				fmt.Printf("    Error pushing updated %s: %v\n", name, err)
			}
		}
	}

	fmt.Println("\n=== Release " + version + " complete ===")
}
