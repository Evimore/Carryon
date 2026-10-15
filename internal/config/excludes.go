package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// excludesHeader starts a new excludes.txt.
const excludesHeader = `# Paths Carryon leaves out of the backup: one restic pattern per line.
# Lines starting with # are comments, and comments must be on their own line.
# A pattern starting with / matches from the top of the system; one without matches anywhere.
# Carryon also leaves out the defaults in %s; to back one of
# those up after all, add it to backup.skip_default_excludes in config.toml.
# Add lines here with "carryon exclude PATH".
`

// ReadExcludes returns the patterns in an exclude file, without comments and blank lines. A
// missing file has none.
func ReadExcludes(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var pats []string
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, "#") {
			pats = append(pats, ln)
		}
	}
	return pats, nil
}

// AddExclude appends a pattern to the exclude file at path, creating the file with an explaining
// header (which names defaultsPath, the shipped default excludes) when it doesn't exist yet. It
// returns false without changing anything when the file already has the pattern.
func AddExclude(path, defaultsPath, pattern string) (bool, error) {
	pattern = strings.TrimSpace(pattern)
	if err := checkPattern(pattern); err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data = fmt.Appendf(nil, excludesHeader, defaultsPath)
	} else if err != nil {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	have, err := ReadExcludes(path)
	if err != nil {
		return false, err
	}
	for _, p := range have {
		if p == pattern {
			return false, nil
		}
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, pattern+"\n"...)
	if err := WriteFileAtomic(path, data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// checkPattern refuses what can't be one line of an exclude file.
func checkPattern(p string) error {
	switch {
	case p == "":
		return errors.New("the pattern is empty")
	case strings.ContainsAny(p, "\n\r\x00"):
		return errors.New("a pattern must fit on one line")
	case strings.HasPrefix(p, "#"):
		return errors.New("a pattern can't start with #: restic would read it as a comment")
	}
	return nil
}
