package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Modes accepted by Pipeline.Run.
const (
	modeTip     = "tip"
	modeRelease = "release"
	modeCheck   = "check"
)

// Execute runs the CLI and returns a process exit code.
func Execute(appVersion string) int {
	ctx, stop := signalContext()
	defer stop()

	err := newRootCmd(appVersion).ExecuteContext(ctx)
	if err != nil && ctx.Err() != nil {
		// The user interrupted: a failure the run happened to record first must not
		// mask the cancellation, which has its own exit code.
		err = context.Canceled
	}
	if err != nil {
		return reportError(err)
	}
	return 0
}

// reportError prints an error, expanding the failing commands a run collected, and
// maps them onto an exit code.
func reportError(err error) int {
	// Check cancellation first: a chained error may carry a context cause, and an
	// interrupt should read as 130 rather than a generic failure.
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "cancelled")
		return 130
	}
	var ce *chainError
	if errors.As(err, &ce) {
		fmt.Fprintf(os.Stderr, "error: %s\n", ce.Error())
		for _, c := range ce.causes {
			fmt.Fprintf(os.Stderr, "  caused by: %s\n", c)
		}
		return 1
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
	return 1
}

// chainError carries the concrete git/go invocations that failed alongside the
// headline error, so a namespace-wide failure points at the command that broke.
type chainError struct {
	msg    string
	causes []error
}

func (e *chainError) Error() string { return e.msg }

// Unwrap exposes the causes so errors.Is still sees a context cancellation that
// arrived after an earlier failure. Without it, Ctrl-C during a run whose journal
// already held a failed command would exit 1 instead of 130.
func (e *chainError) Unwrap() []error { return e.causes }

// wrapWithCauses enriches err with the journal's failed commands, if any.
func wrapWithCauses(err error, journal *Journal) error {
	if err == nil {
		return nil
	}
	ce := &chainError{msg: err.Error()}
	for _, a := range journal.Failures() {
		if a.Err != nil {
			ce.causes = append(ce.causes, fmt.Errorf("%s: %w", a, a.Err))
		}
	}
	if len(ce.causes) == 0 {
		return err
	}
	return ce
}

// signalContext cancels the run on SIGINT/SIGTERM so in-flight git/go commands stop.
func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			fmt.Fprintln(os.Stderr, "\ninterrupted; finishing the current command then stopping")
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

// newRootCmd builds the command tree. The bare command performs a tip check-in
// without tagging, which is the safe default.
func newRootCmd(appVersion string) *cobra.Command {
	d := Defaults()

	root := &cobra.Command{
		Use:   "go-i2p-release",
		Short: "Release harness for the go-i2p module namespace",
		Long: strings.TrimSpace(`
go-i2p-release keeps the go-i2p namespace coherent.

It walks the namespace's repositories in dependency order, points every
in-namespace dependency at the newest commit of its peers, tidies and verifies
each module, and checks the resulting go.mod/go.sum back in.

Running the command with no subcommand performs that tip check-in: modules are
updated to the tip, verified, committed and pushed, but nothing is tagged. That is
the default mode, and it is the safe half of a release -- land the tips first,
then tag once everything builds and tests clean. Use --dry-run to preview, or the
release command to tag.

Configuration resolves in this order: explicit flag, environment variable
(GO_I2P_RELEASE_*), config file (go-i2p-release.yaml), built-in default. The
namespace defaults to $GOPATH/src/github.com/go-i2p.
`),
		Example: `  # Default: bump to tips, verify, commit and push. No tags are created.
  go-i2p-release

  # Preview it first
  go-i2p-release --dry-run

  # Full release: tip check-in, then tag v0.1.8 everywhere and push branches+tags
  go-i2p-release release v0.1.8

  # Refresh only the library layers, leaving the application layer alone
  go-i2p-release --libs

  # Print the resolved release set, order and in-namespace dependencies
  go-i2p-release plan`,
		Version:       appVersion,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMode(cmd, modeTip, "")
		},
	}
	root.SetVersionTemplate(fmt.Sprintf("go-i2p-release %s\n", appVersion))

	// Built-in values are the flag defaults, so viper's own precedence
	// (set flag > env > config file > flag default) is exactly the order documented
	// above and no manual overlay is needed.
	pf := root.PersistentFlags()
	pf.String("namespace", d.Namespace, "directory holding the go-i2p repositories")
	pf.String("prefix", d.Prefix, "module prefix shared by the namespace")
	pf.Bool("dry-run", d.DryRun, "report what would change without touching the repositories")
	pf.Bool("push", d.Push, "publish commits (and tags when releasing) to origin")
	pf.String("verify", d.Verify, "validate after bumping: none, build, or test")
	pf.Bool("sync", d.Sync, "fetch and fast-forward each repository before computing tips")
	pf.Bool("allow-dirty", d.AllowDirty, "proceed even when repositories have uncommitted changes")
	pf.String("branch", d.Branch, "override the default branch detected for each repository")
	pf.String("message", d.Message, "commit message used for dependency check-ins")
	pf.StringSlice("only", d.Only, "restrict to these repositories (repeatable or comma-separated)")
	pf.StringSlice("exclude", d.Exclude, "drop these repositories (repeatable or comma-separated)")
	pf.Bool("all", d.All, "include every module under the namespace, not just the known release set")
	pf.Bool("libs", d.Libs, "limit to the library layers (everything below the application layer)")
	pf.Bool("clients", d.Clients, "limit to the application layer")
	pf.Bool("post-tag-bump", d.PostTagBump, "after tagging, rewrite dependencies to the tag and check that in too")
	pf.String("tag-existing", d.TagExisting, "when the release tag exists: fail, skip, or recreate")
	pf.Bool("standalone", d.Standalone, "resolve each module on its own, ignoring any go.work (GOWORK=off)")
	pf.Bool("private", d.Private, "bypass the module proxy for namespace modules (sets GOPRIVATE)")
	pf.Duration("git-timeout", d.GitTimeout, "timeout for a single git command")
	pf.Duration("go-timeout", d.GoTimeout, "timeout for a single go command")
	pf.BoolP("verbose", "v", d.Verbose, "echo the output of every command")
	pf.Bool("quiet", d.Quiet, "suppress progress lines")

	root.AddCommand(newTipCmd(), newReleaseCmd(), newPlanCmd(), newInitConfigCmd())
	return root
}

// Defaults returns the built-in configuration: a dry-run tip check-in against the
// go-i2p namespace in GOPATH, verified with a build and a test pass.
func Defaults() *Config {
	ns, err := DefaultNamespace()
	if err != nil {
		ns = ""
	}
	return &Config{
		Namespace: ns,
		Prefix:    DefaultPrefix,
		// The default run updates every module to its peers' tips, verifies it and
		// checks it in, without tagging. Tagging is what `release` is for.
		DryRun:      false,
		Push:        true,
		Verify:      verifyTest,
		Sync:        true,
		TagExisting: tagFail,
		Standalone:  true,
		Private:     true,
		GitTimeout:  2 * time.Minute,
		GoTimeout:   15 * time.Minute,
		Message:     DefaultMessage,
	}
}

// initViper binds the command's flags plus environment and config-file sources.
func initViper(cmd *cobra.Command) error {
	viper.SetEnvPrefix("GO_I2P_RELEASE")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	viper.AutomaticEnv()

	flags := cmd.Flags()
	if err := BindFlags(flags); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}

	viper.SetConfigName("go-i2p-release")
	viper.SetConfigType("yaml")
	for _, dir := range configSearchPaths(flags) {
		viper.AddConfigPath(dir)
	}
	if err := viper.ReadInConfig(); err != nil {
		var nf viper.ConfigFileNotFoundError
		if !errors.As(err, &nf) {
			fmt.Fprintf(os.Stderr, "warning: reading config file: %v\n", err)
		}
	}
	return nil
}

// configSearchPaths looks in the working directory, the namespace, and the usual
// user-level config locations.
func configSearchPaths(flags *pflag.FlagSet) []string {
	paths := []string{"."}
	if f := flags.Lookup("namespace"); f != nil && f.Value.String() != "" {
		paths = append(paths, f.Value.String())
	}
	if ns, err := DefaultNamespace(); err == nil {
		paths = append(paths, ns)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config"), home)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "go-i2p-release"))
	}
	return paths
}

// resolveConfig builds the effective configuration for this invocation.
func resolveConfig(cmd *cobra.Command) (*Config, error) {
	if err := initViper(cmd); err != nil {
		return nil, err
	}
	return LoadConfig()
}

// prepare resolves configuration, discovers the release set and builds the pipeline.
func prepare(cmd *cobra.Command, mode, version string) (*Pipeline, *Journal, *Config, error) {
	cfg, err := resolveConfig(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	if version != "" {
		cfg.Version = version
	}
	if mode != modeRelease {
		// Tip check-ins never tag, so a stray VERSION from the environment must not
		// make the summary claim a release happened.
		cfg.Version = ""
	}
	if mode == modeCheck {
		// plan is strictly read-only regardless of configuration: it must never
		// checkout, merge, commit or push.
		cfg.DryRun = true
		cfg.Push = false
	}
	if mode == modeRelease && cfg.Version == "" {
		return nil, nil, nil, fmt.Errorf("a version is required: go-i2p-release release v0.1.8 (or GO_I2P_RELEASE_VERSION=v0.1.8)")
	}
	if err := cfg.normalize(); err != nil {
		return nil, nil, nil, err
	}

	scan, err := Discover(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	j := NewJournal(cmd.OutOrStdout())
	j.SetQuiet(cfg.Quiet)
	p := NewPipeline(cfg, scan, j)
	if mode == modeCheck {
		p.SetReadOnly()
	}
	if !cfg.Quiet {
		desc := "tip check-in, no tags"
		if mode == modeRelease {
			desc = "release " + cfg.Version
		}
		fmt.Fprintf(cmd.OutOrStdout(), "go-i2p-release: %d repositories in %s (%s)\n",
			len(scan.Repos), cfg.Namespace, desc)
		switch {
		case cfg.DryRun:
			fmt.Fprintln(cmd.OutOrStdout(), "dry run: nothing is committed, tagged or pushed.")
		case cfg.Push:
			fmt.Fprintln(cmd.OutOrStdout(), "will commit and push to origin; verification must pass first. Use --push=false to keep changes local.")
		default:
			fmt.Fprintln(cmd.OutOrStdout(), "will commit locally only (use --push to publish).")
		}
		if mode != modeRelease {
			fmt.Fprintln(cmd.OutOrStdout(), "no tags will be created; use the release command to tag.")
		}
	}
	return p, j, cfg, nil
}

func runMode(cmd *cobra.Command, mode, version string) error {
	p, j, cfg, err := prepare(cmd, mode, version)
	if err != nil {
		return err
	}
	start := time.Now()
	runErr := p.Run(cmd.Context(), mode)
	if !cfg.Quiet {
		fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", p.Summary(start))
	}
	if runErr != nil {
		return wrapWithCauses(runErr, j)
	}
	if f := j.Failures(); len(f) > 0 && !cfg.DryRun {
		return wrapWithCauses(fmt.Errorf("%d commands failed during the run", len(f)), j)
	}
	return nil
}

func newTipCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "tip",
		Aliases: []string{"checkin", "update"},
		Short:   "Bump dependencies to branch tips, verify and check in (no tags)",
		Long: strings.TrimSpace(`
Bump every in-namespace dependency to the tip of its default branch, tidy and
verify each module, then commit go.mod/go.sum in every repository that changed.
No tags are created. This is what the bare command does.
`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMode(cmd, modeTip, "")
		},
	}
}

func newReleaseCmd() *cobra.Command {
	var version string
	c := &cobra.Command{
		Use:   "release [version]",
		Short: "Bump to tips, tag every repository and push",
		Long: strings.TrimSpace(`
Perform a namespace-wide release: bump dependencies to their tips, verify, check
the result in, create an annotated tag for the version in every repository, and
push branches and tags.

With --post-tag-bump a second pass rewrites the dependencies from pseudo-versions
to the new tag and checks that in as well, so the released go.mod files reference
releases rather than commits.
`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// With no argument the version comes from --version, VERSION or the
			// config file, all of which LoadConfig already folds in.
			v := ""
			if len(args) > 0 {
				v = args[0]
			} else if version != "" {
				v = version
			}
			return runMode(cmd, modeRelease, v)
		},
	}
	c.Flags().StringVarP(&version, "version", "V", "", "release version to tag (may also be given as the first argument)")
	return c
}

func newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "plan",
		Aliases: []string{"check", "ls"},
		Short:   "Show the resolved release set, order and dependencies",
		Long: strings.TrimSpace(`
Inspect the namespace and print the repositories that would be released, each one's
default branch, current tip and in-namespace dependencies. Makes no changes.
`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMode(cmd, modeCheck, "")
		},
	}
}

func newInitConfigCmd() *cobra.Command {
	var force, stdout bool
	c := &cobra.Command{
		Use:   "init-config",
		Short: "Write a documented go-i2p-release.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if stdout {
				_, err := fmt.Fprint(cmd.OutOrStdout(), sampleConfig)
				return err
			}
			path := filepath.Join(".", "go-i2p-release.yaml")
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
			if err := os.WriteFile(path, []byte(sampleConfig), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	c.Flags().BoolVar(&stdout, "stdout", false, "print to standard output instead of writing a file")
	return c
}

const sampleConfig = `# go-i2p-release configuration.
# Explicit flags override environment variables (GO_I2P_RELEASE_*), which override
# this file, which overrides the built-in defaults.
namespace: ""                     # default: $GOPATH/src/github.com/go-i2p
prefix: github.com/go-i2p         # module prefix shared by the namespace
dry-run: false                    # true: report only, change nothing
push: true                        # publish commits (and tags when releasing)
verify: test                      # none | build | test
sync: true                        # fetch and fast-forward before computing tips
allow-dirty: false                # refuse to run on uncommitted changes
standalone: true                  # resolve each module on its own (GOWORK=off)
private: true                     # resolve namespace modules from the host, not the proxy
tag-existing: fail                # fail | skip | recreate (release mode only)
post-tag-bump: false              # after tagging, rewrite deps to the tag and check in
only: []                          # restrict to these repositories
exclude: []                       # drop these repositories
git-timeout: 2m
go-timeout: 15m
`
