package record

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Evimore/carryon/internal/sysexec"
)

// The identity the record's history is written under.
const (
	gitName  = "carryon"
	gitEmail = "carryon@localhost"
)

// gitRepo runs git on the history of the record in dir.
//
// Carryon usually runs as root, whose own git settings (signing, hooks, a pager, a default
// branch...) have nothing to do with the record. So git runs through env with its global and
// system settings switched off, the repository given explicitly, and a fixed identity:
//
//	env GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_DIR=DIR/.git ... git --no-pager -C DIR -c ... ARGS
type gitRepo struct {
	run sysexec.Runner
	dir string
}

// gitFor returns the record's git, or false when there's no runner or git isn't installed.
func gitFor(r sysexec.Runner, dir string) (gitRepo, bool) {
	if r == nil || !r.Exists("git") {
		return gitRepo{}, false
	}
	return gitRepo{run: r, dir: dir}, true
}

func (g gitRepo) gitDir() string { return filepath.Join(g.dir, ".git") }

// output runs git with args. A non-zero when sets the dates of a new commit.
func (g gitRepo) output(ctx context.Context, when time.Time, args ...string) ([]byte, error) {
	argv := []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_DIR=" + g.gitDir(),
		"GIT_WORK_TREE=" + g.dir,
		"GIT_INDEX_FILE=" + filepath.Join(g.gitDir(), "index"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=" + gitName,
		"GIT_AUTHOR_EMAIL=" + gitEmail,
		"GIT_COMMITTER_NAME=" + gitName,
		"GIT_COMMITTER_EMAIL=" + gitEmail,
	}
	if !when.IsZero() {
		d := when.Format(time.RFC3339)
		argv = append(argv, "GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
	}
	argv = append(argv, "git", "--no-pager", "-C", g.dir,
		"-c", "commit.gpgSign=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "log.showSignature=false",
	)
	return g.run.Output(ctx, "env", append(argv, args...)...)
}

// exists reports whether the record has a git repository.
func (g gitRepo) exists() (bool, error) {
	_, err := os.Stat(g.gitDir())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// hasCommits reports whether the history has at least one commit.
func (g gitRepo) hasCommits(ctx context.Context) (bool, error) {
	_, err := g.output(ctx, time.Time{}, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	if code, ok := sysexec.ExitCode(err); ok && code == 1 {
		return false, nil
	}
	return err == nil, err
}
