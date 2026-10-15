package record

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/sysexec"
)

// Reasons History has nothing to show.
var (
	ErrNoHistory  = errors.New("the system record has no history yet")
	ErrNoGit      = errors.New("git isn't installed")
	ErrNotAllowed = errors.New("the system record is only readable by root")
)

// Commit is one entry in the record's history.
type Commit struct {
	Hash    string    `json:"commit"`
	Date    time.Time `json:"date"`
	Subject string    `json:"subject"`
	Changes []string  `json:"changes,omitempty"` // "+ ", "- " and "~ " lines, as ChangeLines writes them
}

// History returns up to n commits of the record in dir, newest first. n <= 0 means all of them.
//
// It fails with ErrNotAllowed when the record can't be read (it's root's), ErrNoGit when git is
// missing and ErrNoHistory when there's no commit yet.
func History(ctx context.Context, dir string, git sysexec.Runner, n int) ([]Commit, error) {
	for _, p := range []string{dir, filepath.Join(dir, ".git")} {
		_, err := os.Stat(p)
		if errors.Is(err, os.ErrPermission) {
			return nil, ErrNotAllowed
		}
		if errors.Is(err, os.ErrNotExist) {
			if _, ok := gitFor(git, dir); !ok {
				return nil, ErrNoGit
			}
			return nil, ErrNoHistory
		}
		if err != nil {
			return nil, err
		}
	}
	g, ok := gitFor(git, dir)
	if !ok {
		return nil, ErrNoGit
	}
	has, err := g.hasCommits(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the history of %s: %w", dir, err)
	}
	if !has {
		return nil, ErrNoHistory
	}
	args := []string{"log", "--format=%H%x1f%aI%x1f%s%x1f%b%x1e"}
	if n > 0 {
		args = append(args, "-n", strconv.Itoa(n))
	}
	out, err := g.output(ctx, time.Time{}, args...)
	if err != nil {
		return nil, fmt.Errorf("reading the history of %s: %w", dir, err)
	}
	return parseLog(string(out))
}

// parseLog reads git log output in the format History asks for.
func parseLog(out string) ([]Commit, error) {
	var commits []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected git log output: %q", rec)
		}
		date, err := time.Parse(time.RFC3339, f[1])
		if err != nil {
			return nil, fmt.Errorf("unexpected date in git log: %w", err)
		}
		c := Commit{Hash: f[0], Date: date, Subject: f[2]}
		for _, line := range strings.Split(f[3], "\n") {
			if len(line) > 2 && strings.Contains("+-~", line[:1]) && line[1] == ' ' {
				c.Changes = append(c.Changes, line)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}
