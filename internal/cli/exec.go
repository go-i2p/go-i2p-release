package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Action is a single recorded step of a run.
type Action struct {
	Dir  string
	Args []string
	Err  error
	// Planned marks an action skipped because of --dry-run.
	Planned  bool
	Stdout   string
	Stderr   string
	Duration time.Duration
}

func (a Action) String() string {
	dir := a.Dir
	if dir == "" {
		dir = "."
	}
	return fmt.Sprintf("%s  [%s]", strings.Join(a.Args, " "), dir)
}

// Journal records every action taken (or planned) so a run can be reviewed.
type Journal struct {
	Actions []Action
	out     io.Writer
}

// NewJournal returns a journal that echoes progress to out.
func NewJournal(out io.Writer) *Journal { return &Journal{out: out} }

func (j *Journal) logf(format string, args ...any) {
	if j.out != nil {
		fmt.Fprintf(j.out, format+"\n", args...)
	}
}

// Failures returns the actions that errored.
func (j *Journal) Failures() []Action {
	var out []Action
	for _, a := range j.Actions {
		if a.Err != nil {
			out = append(out, a)
		}
	}
	return out
}

// Planned returns the actions skipped by --dry-run.
func (j *Journal) Planned() []Action {
	var out []Action
	for _, a := range j.Actions {
		if a.Planned {
			out = append(out, a)
		}
	}
	return out
}

// Executor runs external commands under a shared context, honouring --dry-run.
type Executor struct {
	cfg     *Config
	journal *Journal
	env     []string
}

// NewExecutor returns an executor using cfg's environment and journal.
func NewExecutor(cfg *Config, j *Journal) *Executor {
	return &Executor{cfg: cfg, journal: j, env: cfg.Env()}
}

// Run executes args in dir. Mutating commands are recorded as planned, not run, when
// --dry-run is set; readOnly commands always run.
func (e *Executor) Run(ctx context.Context, dir string, readOnly bool, args ...string) (string, error) {
	rec := Action{Dir: dir, Args: args}
	if e.cfg.DryRun && !readOnly {
		rec.Planned = true
		e.journal.Actions = append(e.journal.Actions, rec)
		e.journal.logf("    (dry run) %s", rec)
		return "", nil
	}

	cctx, cancel := context.WithTimeout(ctx, e.timeoutFor(args))
	defer cancel()

	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = e.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	rec.Duration = time.Since(start)
	rec.Stdout = strings.TrimSpace(stdout.String())
	rec.Stderr = strings.TrimSpace(stderr.String())
	if err != nil {
		detail := tail(rec.Stderr)
		if cctx.Err() == context.DeadlineExceeded {
			detail = fmt.Sprintf("timed out after %s", e.timeoutFor(args))
		}
		rec.Err = fmt.Errorf("%v: %s", err, detail)
	}
	e.journal.Actions = append(e.journal.Actions, rec)

	if e.cfg.Verbose {
		e.journal.logf("      %s (%s)", rec, rec.Duration.Round(time.Millisecond))
		if rec.Stdout != "" {
			e.journal.logf("%s", indent(trimLines(rec.Stdout, 40)))
		}
		if err != nil {
			e.journal.logf("      ! %s", tail(rec.Stderr))
		}
	}
	return rec.Stdout, rec.Err
}

func (e *Executor) timeoutFor(args []string) time.Duration {
	if len(args) > 0 && args[0] == "go" {
		return e.cfg.GoTimeout
	}
	return e.cfg.GitTimeout
}

// Quiet runs a read-only command and returns its trimmed standard output without
// recording it as an action. On failure it returns empty output, never stderr: callers
// probe for existence with commands like `git rev-parse --verify`, where the "fatal:"
// diagnostic on stderr must not be mistaken for a result.
func (e *Executor) Quiet(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, e.timeoutFor(args))
	defer cancel()
	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = e.env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), err
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= 8 {
		return s
	}
	return "... " + strings.Join(lines[len(lines)-8:], "\n")
}

func trimLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n      ... (%d more lines)", len(lines)-n)
}

func indent(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n")
}

// FindGoWork walks upward from dir looking for a go.work file. Returns "" if none.
func FindGoWork(dir string) string {
	cur := dir
	for {
		p := filepath.Join(cur, "go.work")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}
