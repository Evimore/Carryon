// Package engine runs restic: it builds restic's command line and environment from the config,
// runs it quietly or with live output, parses its JSON output and turns its failures into a cause
// plus what to do. It also holds the rclone shim that lets restic, running as root, use a user's
// rclone login. See docs/design.md, "Storage".
package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

// RetryLock is how long restic waits for another run's lock before giving up.
const RetryLock = "30m"

// RcloneUserEnv tells the rclone shim whose rclone login to use. The engine sets it in restic's
// environment, and restic passes it on to the rclone it starts.
const RcloneUserEnv = "CARRYON_RCLONE_USER"

// Engine runs restic against one repository.
type Engine struct {
	repo    config.Repository
	dirs    paths.Dirs
	machine string

	// Program is the restic executable, "restic" (looked up on PATH) unless set.
	Program string
	// Self is the carryon executable restic runs as rclone for rclone: repositories. Empty means
	// os.Executable().
	Self string
	// BaseEnv is the environment restic starts from, before the engine's own variables. Nil means
	// os.Environ().
	BaseEnv []string
	// Stderr receives restic's live output (see Live). Nil means os.Stderr.
	Stderr io.Writer
	// Runner starts restic. Nil means running it with os/exec; tests may replace it.
	Runner Runner
	// NoLock passes --no-lock instead of --retry-lock, for read-only commands (snapshots, cat
	// config) that must never write into the repository, not even a lock file: carryon status.
	NoLock bool
}

// New returns an engine for a repository. machine is the name this machine's snapshots are stored
// under.
func New(repo config.Repository, dirs paths.Dirs, machine string) *Engine {
	return &Engine{repo: repo, dirs: dirs, machine: machine, Program: "restic"}
}

// Machine is the name this machine's snapshots are stored under.
func (e *Engine) Machine() string { return e.machine }

// Repository is the repository the engine works on.
func (e *Engine) Repository() config.Repository { return e.repo }

// IsRclone reports whether the repository is reached through rclone.
func (e *Engine) IsRclone() bool { return strings.HasPrefix(e.repo.URL, "rclone:") }

// passwordFile is where the password comes from when the config names neither a file nor a command.
func (e *Engine) passwordFile() string {
	if e.repo.PasswordFile != "" {
		return e.repo.PasswordFile
	}
	return e.dirs.PasswordFile()
}

// effectiveRepo is the repository with the password file filled in when it comes from the default
// place, so messages can name it.
func (e *Engine) effectiveRepo() config.Repository {
	r := e.repo
	if r.PasswordCommand == "" {
		r.PasswordFile = e.passwordFile()
	}
	return r
}

// Variables the engine always sets itself; inherited values would point restic elsewhere.
// RESTIC_PASSWORD in particular would win over the password file.
var ownedEnv = []string{
	"HOME", "RESTIC_REPOSITORY", "RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE",
	"RESTIC_PASSWORD_COMMAND", "RESTIC_CACHE_DIR", RcloneUserEnv,
}

// Env is restic's environment: the base environment with HOME, the repository, the password source
// and the cache folder set.
func (e *Engine) Env() []string {
	base := e.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	env := withoutVars(base, ownedEnv...)
	// systemd services start without HOME, and restic gives up without one.
	env = append(env, "HOME="+home(base), "RESTIC_REPOSITORY="+e.repo.URL)
	if e.repo.PasswordCommand != "" {
		env = append(env, "RESTIC_PASSWORD_COMMAND="+e.repo.PasswordCommand)
	} else {
		env = append(env, "RESTIC_PASSWORD_FILE="+e.passwordFile())
	}
	env = append(env, "RESTIC_CACHE_DIR="+e.dirs.ResticCache())
	if e.IsRclone() && e.repo.RcloneUser != "" {
		env = append(env, RcloneUserEnv+"="+e.repo.RcloneUser)
	}
	return env
}

// home is the HOME restic gets: the inherited one, else the current user's, else "/".
func home(env []string) string {
	if h := lookupVar(env, "HOME"); h != "" {
		return h
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return "/"
}

// Args is restic's full argument list for a command: the default options, then args. With NoLock
// the options say not to lock the repository, so args must be a command restic runs without one.
func (e *Engine) Args(args ...string) ([]string, error) {
	out := []string{"--retry-lock", RetryLock}
	if e.NoLock {
		out = []string{"--no-lock"}
	}
	if e.IsRclone() {
		prog, err := e.rcloneProgram()
		if err != nil {
			return nil, err
		}
		out = append(out, "-o", "rclone.program="+prog)
	}
	return append(out, args...), nil
}

// rcloneProgram is the rclone.program option: carryon's rclone shim. restic splits the option like
// a shell (quotes group, whitespace and backslashes separate), runs the first word with the rest,
// then rclone.args ("serve restic --stdio --b2-hard-delete" by default), then the remote.
func (e *Engine) rcloneProgram() (string, error) {
	self := e.Self
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return "", fmt.Errorf("finding the carryon program for rclone: %w", err)
		}
	}
	if strings.ContainsAny(self, `'"\`) {
		return "", fmt.Errorf("carryon is installed at %q, which restic can't pass to rclone: move it to a path without quotes or backslashes", self)
	}
	if strings.ContainsFunc(self, isSpace) {
		self = "'" + self + "'"
	}
	return self + " rclone-shim", nil
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// Cmd is one restic run.
type Cmd struct {
	Path   string
	Args   []string // without the program name
	Env    []string
	Stdin  io.Reader // nil means no input
	Stdout io.Writer
	Stderr io.Writer
}

// Command builds a restic run for args. The caller sets its input and output.
func (e *Engine) Command(args ...string) (Cmd, error) {
	full, err := e.Args(args...)
	if err != nil {
		return Cmd{}, err
	}
	prog := e.Program
	if prog == "" {
		prog = "restic"
	}
	return Cmd{Path: prog, Args: full, Env: e.Env()}, nil
}

func (e *Engine) runner() Runner {
	if e.Runner != nil {
		return e.Runner
	}
	return ExecRunner
}

func (e *Engine) stderr() io.Writer {
	if e.Stderr != nil {
		return e.Stderr
	}
	return os.Stderr
}

// withoutVars returns env without the named variables.
func withoutVars(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, n := range names {
			if name == n {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// lookupVar returns the last value of a variable in env.
func lookupVar(env []string, name string) string {
	val := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			val = v
		}
	}
	return val
}

// Run runs restic quietly and captures its output. The error is non-nil only when restic couldn't
// be started; check Result.Code or Result.Err for restic's own failures.
func (e *Engine) Run(ctx context.Context, args ...string) (Result, error) {
	c, err := e.Command(args...)
	if err != nil {
		return Result{}, err
	}
	var stdout, stderr strings.Builder
	c.Stdout, c.Stderr = &stdout, &stderr
	code, err := e.runner()(ctx, c)
	if err != nil {
		return Result{}, e.startError(err)
	}
	return Result{Args: args, Code: code, Stdout: stdout.String(), Stderr: stderr.String(), repo: e.effectiveRepo()}, nil
}

// TailLines is how many of restic's last stderr lines Live keeps to explain a failure.
const TailLines = 30

// Live runs restic with its stderr (progress and errors) shown as it comes, on Engine.Stderr, and
// keeps the last TailLines lines in Result.Stderr. stdout goes to the given writer, or into
// Result.Stdout when it is nil.
func (e *Engine) Live(ctx context.Context, stdout io.Writer, args ...string) (Result, error) {
	c, err := e.Command(args...)
	if err != nil {
		return Result{}, err
	}
	var captured strings.Builder
	c.Stdout = stdout
	if stdout == nil {
		c.Stdout = &captured
	}
	tail := newTailWriter(e.stderr(), TailLines)
	c.Stderr = tail
	fmt.Fprintf(e.stderr(), "$ restic %s\n", strings.Join(args, " "))
	code, err := e.runner()(ctx, c)
	tail.Flush()
	if err != nil {
		return Result{}, e.startError(err)
	}
	return Result{Args: args, Code: code, Stdout: captured.String(), Stderr: tail.String(), repo: e.effectiveRepo()}, nil
}

// startError explains why restic couldn't be started.
func (e *Engine) startError(err error) error {
	return &Error{Problem: Problem{
		Cause: fmt.Sprintf("restic couldn't be started: %v", err),
		Fix:   "install restic " + MinVersion + " or newer (on Arch: sudo pacman -S restic)",
	}, Code: -1}
}

// Result is a finished restic run.
type Result struct {
	Args   []string // the command as given, without the engine's default options
	Code   int      // exit code; 128+n when killed by signal n
	Stdout string
	Stderr string // all of it for Run, the last lines for Live

	repo config.Repository
}

// Err returns nil when restic succeeded, otherwise an *Error explaining what went wrong.
func (r Result) Err() error {
	if r.Code == 0 {
		return nil
	}
	return &Error{Problem: Classify(r.Code, r.Stderr, r.repo), Code: r.Code, Args: r.Args}
}
