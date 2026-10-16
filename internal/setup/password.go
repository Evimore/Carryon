package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
)

// PasswordLength is how many characters a generated password has, like the prototype's.
const PasswordLength = 40

const passwordChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// GeneratePassword returns PasswordLength random letters and digits read from r.
func GeneratePassword(r io.Reader) (string, error) {
	// Rejection sampling keeps every character equally likely: 248 is the largest multiple of 62
	// that fits in a byte.
	limit := byte(256 - 256%len(passwordChars))
	out := make([]byte, 0, PasswordLength)
	buf := make([]byte, 64)
	for len(out) < PasswordLength {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("generating a password: %w", err)
		}
		for _, b := range buf {
			if b < limit && len(out) < PasswordLength {
				out = append(out, passwordChars[int(b)%len(passwordChars)])
			}
		}
	}
	return string(out), nil
}

// ReadPasswordFile reads a password from a file: its first line, without the line break.
func ReadPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the password from %s: %w", path, err)
	}
	pw, _, _ := strings.Cut(string(data), "\n")
	pw = strings.TrimRight(pw, "\r")
	if pw == "" {
		return "", fmt.Errorf("%s holds no password (its first line is empty)", path)
	}
	return pw, nil
}

// checkPassword refuses what can't be stored as the first line of a password file.
func checkPassword(pw string) error {
	switch {
	case pw == "":
		return errors.New("the password is empty")
	case strings.ContainsAny(pw, "\n\r"):
		return errors.New("the password must fit on one line")
	}
	return nil
}

// secret is where the repository password comes from while init runs: a candidate in a staging
// file next to the real one, or a command. Nothing replaces the real password file until commit.
type secret struct {
	password string // empty when command is set
	command  string // a password_command such as systemd-creds
	staging  string // the staging file holding password
}

// newSecret stages a password in a root-only file next to the real password file.
func (e *Env) newSecret(pw string) (*secret, error) {
	if err := checkPassword(pw); err != nil {
		return nil, err
	}
	staging := filepath.Join(filepath.Dir(e.Dirs.PasswordFile()), ".repository-password.staging")
	if err := writePrivate(staging, []byte(pw+"\n")); err != nil {
		return nil, err
	}
	return &secret{password: pw, staging: staging}, nil
}

// repo is the repository at url, opened with this secret.
func (s *secret) repo(url string) config.Repository {
	if s.command != "" {
		return config.Repository{URL: url, PasswordCommand: s.command}
	}
	return config.Repository{URL: url, PasswordFile: s.staging}
}

// discard removes the staging file.
func (s *secret) discard() {
	if s != nil && s.staging != "" {
		os.Remove(s.staging)
	}
}

// commit stores the password in the real password file (root only) and returns the repository
// settings that read it. An older password file with a different password is kept as .old, so a
// wrong choice never loses the password of another repository.
func (e *Env) commit(s *secret, url string) (config.Repository, error) {
	if s.command != "" {
		return config.Repository{URL: url, PasswordCommand: s.command}, nil
	}
	file := e.Dirs.PasswordFile()
	old, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return config.Repository{}, fmt.Errorf("reading %s: %w", file, err)
	case strings.TrimRight(string(old), "\r\n") != s.password:
		if err := writePrivate(file+".old", old); err != nil {
			return config.Repository{}, err
		}
		e.Log.Warn("Kept the previous password as %s.old, since the new one is different.", file)
	}
	if err := writePrivate(file, []byte(s.password+"\n")); err != nil {
		return config.Repository{}, err
	}
	s.discard()
	return config.Repository{URL: url, PasswordFile: file}, nil
}

// repoState is what a repository URL holds.
type repoState int

const (
	repoMissing  repoState = iota // nothing there yet: init creates it
	repoOpens                     // a repository the password opens
	repoWrongKey                  // a repository the password doesn't open
)

// probe tells whether there's a repository at the engine's URL and whether its password opens it.
// Other failures (offline, login expired) come back as errors with restic's cause and a fix.
func probe(ctx context.Context, eng *engine.Engine) (repoState, string, error) {
	r, err := eng.Run(ctx, "cat", "config")
	if err != nil {
		return 0, "", err
	}
	if r.Code == 0 {
		c, err := engine.ParseRepoConfig([]byte(r.Stdout))
		if err != nil {
			return 0, "", err
		}
		return repoOpens, c.ID, nil
	}
	switch {
	case engine.IsBadPassword(r.Code, r.Stderr):
		return repoWrongKey, "", nil
	case engine.IsNoRepo(r.Code, r.Stderr):
		return repoMissing, "", nil
	}
	return 0, "", r.Err()
}
