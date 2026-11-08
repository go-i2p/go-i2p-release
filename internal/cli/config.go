package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/mod/semver"
)

// Verify modes for --verify.
const (
	verifyNone  = "none"
	verifyBuild = "build"
	verifyTest  = "test"
)

// Existing-tag policies for --tag-existing.
const (
	tagFail     = "fail"
	tagSkip     = "skip"
	tagRecreate = "recreate"
)

// Config is the fully resolved configuration for a run. Every field is settable
// through a flag, an environment variable or the config file; resolution order is
// flag > env > config file > built-in default.
type Config struct {
	// Namespace is the directory holding the go-i2p repositories.
	Namespace string
	// Prefix is the module prefix shared by every repository in the namespace.
	Prefix string
	// Version is the release tag applied by the release command.
	Version string

	// DryRun reports what would happen without changing any repository.
	DryRun bool
	// Push publishes branches (and tags) to origin. Ignored when DryRun.
	// The default is true: a tip check-in is meant to land, and verification
	// (build+test) gates it, so a bad tip never reaches the shared namespace.
	Push bool

	// Verify is the post-bump validation depth: none, build or test.
	Verify string
	// Sync fetches each repository and fast-forwards its working tree to the tip.
	Sync bool
	// AllowDirty proceeds on repositories with uncommitted changes.
	AllowDirty bool

	// Only restricts the release set to the named repositories.
	Only []string
	// Exclude drops the named repositories from the release set.
	Exclude []string
	// All takes every module discovered under the namespace, not just the known release set.
	All bool
	// Libs limits the release set to the library layers.
	Libs bool
	// Clients limits the release set to the application layer.
	Clients bool

	// TagExisting is the policy when the release tag already exists.
	TagExisting string
	// Branch overrides the detected default branch of every repository.
	Branch string
	// Message is the commit message used for dependency check-ins.
	Message string
	// PostTagBump rewrites dependencies to the new tag after tagging, then checks that in.
	PostTagBump bool

	// Standalone neutralizes any go.work found above the namespace so that each
	// module is resolved on its own.
	Standalone bool
	// Private forces Go to fetch namespace modules straight from the host instead
	// of the module proxy, so freshly pushed tips resolve immediately.
	Private bool

	// GitTimeout bounds a single git command; GoTimeout bounds a single go command.
	GitTimeout time.Duration
	GoTimeout  time.Duration

	// Verbose echoes command output; Quiet suppresses progress lines.
	Verbose bool
	Quiet   bool

	// warnings collects non-fatal notes surfaced during normalization.
	warnings []string
}

// Warnf records a message for the run summary.
func (c *Config) Warnf(format string, args ...any) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

// Warnings returns recorded non-fatal notes.
func (c *Config) Warnings() []string { return c.warnings }

// BindFlags mirrors a flag set into viper so environment variables and config-file
// keys can supply the same values. Flags that were not explicitly set fall through
// to env, then file, then the flag's own default.
func BindFlags(flags *pflag.FlagSet) error {
	if err := viper.BindPFlags(flags); err != nil {
		return err
	}
	// Backwards compatibility with the pre-cobra tool's environment interface.
	for key, env := range map[string]string{
		"version":   "VERSION",
		"dry-run":   "DRY_RUN",
		"libs":      "LIBS",
		"namespace": "GO_I2P_DIR",
	} {
		if err := viper.BindEnv(key, env); err != nil {
			return err
		}
	}
	return nil
}

// LoadConfig projects viper onto Config and validates the result.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		Namespace:   viper.GetString("namespace"),
		Prefix:      viper.GetString("prefix"),
		Version:     viper.GetString("version"),
		DryRun:      viper.GetBool("dry-run"),
		Push:        viper.GetBool("push"),
		Verify:      strings.ToLower(viper.GetString("verify")),
		Sync:        viper.GetBool("sync"),
		AllowDirty:  viper.GetBool("allow-dirty"),
		Only:        stringList("only"),
		Exclude:     stringList("exclude"),
		All:         viper.GetBool("all"),
		Libs:        viper.GetBool("libs"),
		Clients:     viper.GetBool("clients"),
		TagExisting: strings.ToLower(viper.GetString("tag-existing")),
		Branch:      viper.GetString("branch"),
		Message:     viper.GetString("message"),
		PostTagBump: viper.GetBool("post-tag-bump"),
		Standalone:  viper.GetBool("standalone"),
		Private:     viper.GetBool("private"),
		GitTimeout:  viper.GetDuration("git-timeout"),
		GoTimeout:   viper.GetDuration("go-timeout"),
		Verbose:     viper.GetBool("verbose"),
		Quiet:       viper.GetBool("quiet"),
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// stringList reads a key that may arrive as a []string (flag) or a delimited
// string (environment variable, or a config-file scalar).
func stringList(key string) []string {
	switch v := viper.Get(key).(type) {
	case nil:
		return nil
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return splitList(fmt.Sprint(v))
	}
}

func (c *Config) normalize() error {
	if c.Namespace == "" {
		ns, err := DefaultNamespace()
		if err != nil {
			return err
		}
		c.Namespace = ns
	}
	abs, err := filepath.Abs(c.Namespace)
	if err != nil {
		return fmt.Errorf("resolving namespace %q: %w", c.Namespace, err)
	}
	c.Namespace = abs
	info, err := os.Stat(c.Namespace)
	if err != nil {
		return fmt.Errorf("namespace %s: %w (pass --namespace)", c.Namespace, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("namespace %s is not a directory", c.Namespace)
	}

	c.Prefix = strings.Trim(strings.TrimSpace(c.Prefix), "/")
	if c.Prefix == "" {
		c.Prefix = DefaultPrefix
	}
	if !strings.Contains(c.Prefix, "/") {
		return fmt.Errorf("--prefix %q must be a module path such as github.com/go-i2p", c.Prefix)
	}

	switch c.Verify {
	case verifyNone, verifyBuild, verifyTest:
	case "", "full", "yes", "true":
		c.Verify = verifyTest
	case "no", "false", "off":
		c.Verify = verifyNone
	default:
		return fmt.Errorf("--verify must be one of %s, %s, %s", verifyNone, verifyBuild, verifyTest)
	}

	switch c.TagExisting {
	case tagFail, tagSkip, tagRecreate:
	case "":
		c.TagExisting = tagFail
	default:
		return fmt.Errorf("--tag-existing must be one of %s, %s, %s", tagFail, tagSkip, tagRecreate)
	}

	if c.Libs && c.Clients {
		return fmt.Errorf("--libs and --clients are mutually exclusive")
	}
	if c.GitTimeout <= 0 {
		c.GitTimeout = 2 * time.Minute
	}
	if c.GoTimeout <= 0 {
		c.GoTimeout = 15 * time.Minute
	}
	if strings.TrimSpace(c.Message) == "" {
		c.Message = DefaultMessage
	}
	if c.Version != "" {
		v, err := NormalizeVersion(c.Version)
		if err != nil {
			return err
		}
		c.Version = v
	}
	return nil
}

// NormalizeVersion adds the leading "v" Go requires and validates semantic versioning.
func NormalizeVersion(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("version is required")
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return "", fmt.Errorf("%q is not a valid semantic version (for example v0.1.7)", v)
	}
	return v, nil
}

// splitList accepts both "a,b" and a single name.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// DefaultNamespace resolves the namespace from the Go workspace layout:
// $GOPATH/src/github.com/go-i2p, falling back to $HOME/go/src/github.com/go-i2p.
func DefaultNamespace() (string, error) {
	root := os.Getenv("GOPATH")
	if root == "" {
		if out, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
			root = strings.TrimSpace(string(out))
		}
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate a default namespace: %w", err)
		}
		root = filepath.Join(home, "go")
	}
	if list := filepath.SplitList(root); len(list) > 0 {
		root = list[0]
	}
	return filepath.Join(root, "src", DefaultPrefix), nil
}

// Env returns the environment used for child go/git processes.
func (c *Config) Env() []string {
	env := os.Environ()
	set := func(k, v string) {
		p := k + "="
		for i, e := range env {
			if strings.HasPrefix(e, p) {
				env[i] = p + v
				return
			}
		}
		env = append(env, p+v)
	}
	// A go.work in the namespace lists only some modules, which makes
	// `go build ./...` fail in the others. Pin every module to itself unless asked
	// to honour the workspace.
	if c.Standalone {
		set("GOWORK", "off")
	} else {
		set("GOWORK", "auto")
	}
	// The tool edits dependency requirements, so module mode must allow updates.
	set("GOFLAGS", "-mod=mod")
	if c.Private {
		// Freshly pushed tips are not in the public proxy yet; fetch them from
		// the host and skip the checksum database for them.
		set("GOPRIVATE", c.Prefix+"/*")
	}
	return env
}
