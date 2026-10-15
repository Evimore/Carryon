package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
)

// UserConfigPath is where a user keeps their own settings, relative to their home folder.
const UserConfigPath = ".config/carryon/config.toml"

// maxUserConfigSize is the most Carryon reads of a user's settings file.
const maxUserConfigSize = 64 << 10

// UserConfig is one user's own settings (~/.config/carryon/config.toml). They only narrow what
// the backup reads in that user's home; they can never add anything outside it.
type UserConfig struct {
	// Excludes are restic patterns for paths in the home folder to leave out. After LoadUser,
	// every entry is an absolute pattern inside the home folder.
	Excludes []string   `toml:"excludes"`
	Notify   UserNotify `toml:"notify"`
}

// UserNotify is how problems reach this user.
type UserNotify struct {
	Desktop bool `toml:"desktop"` // show desktop notifications
}

// DefaultUser returns the settings of a user without a settings file.
func DefaultUser() UserConfig {
	return UserConfig{Notify: UserNotify{Desktop: true}}
}

// LoadUser reads the settings of the user whose home folder is home and whose user ID is uid.
//
// It runs as root on a file the user controls, so it refuses a settings file that is reached
// through a symlink leading out of the home folder, that isn't a regular file, that someone else
// owns, or that is unreasonably big. Exclude entries outside the home folder (through "..", an
// absolute path elsewhere, or a symlink leading out) are left out, with a warning each. A missing
// file gives DefaultUser and no error. On an error, callers should use DefaultUser.
func LoadUser(home string, uid int) (UserConfig, []string, error) {
	cfg := DefaultUser()
	if !filepath.IsAbs(home) {
		return cfg, nil, fmt.Errorf("home folder %q isn't an absolute path", home)
	}
	home = filepath.Clean(home)
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return cfg, nil, fmt.Errorf("reading the settings in %s: %w", home, err)
	}
	path := filepath.Join(home, UserConfigPath)

	root, err := os.OpenRoot(realHome)
	if err != nil {
		return cfg, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer root.Close()
	// os.Root refuses any path, through ".." or symlinks, that leads out of the home folder.
	f, err := root.Open(UserConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil, nil
	}
	if err != nil {
		return cfg, nil, fmt.Errorf("not reading %s: it leads outside the home folder or can't be opened (%w)", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cfg, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return cfg, nil, fmt.Errorf("not reading %s: it isn't a regular file", path)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != uid && sys.Uid != 0 {
		return cfg, nil, fmt.Errorf("not reading %s: it belongs to another user", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxUserConfigSize+1))
	if err != nil {
		return cfg, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > maxUserConfigSize {
		return cfg, nil, fmt.Errorf("not reading %s: it is bigger than %d KB", path, maxUserConfigSize>>10)
	}

	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return DefaultUser(), nil, fmt.Errorf("%s: %w", path, err)
	}
	var warnings []string
	for _, k := range md.Undecoded() {
		warnings = append(warnings, fmt.Sprintf("%s: unknown setting %q (ignored)", path, k.String()))
	}
	var excludes []string
	for _, e := range cfg.Excludes {
		p, err := homePattern(home, realHome, e)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: exclude %q ignored: %v", path, e, err))
			continue
		}
		excludes = append(excludes, p)
	}
	cfg.Excludes = excludes
	return cfg, warnings, nil
}

// homePattern turns a user's exclude entry into an absolute restic pattern inside home. Entries
// may start with "~/", be absolute, or be relative to home. realHome is home with symlinks
// resolved.
func homePattern(home, realHome, entry string) (string, error) {
	e := strings.TrimSpace(entry)
	switch {
	case e == "":
		return "", errors.New("it is empty")
	case strings.ContainsAny(e, "\n\r\x00"):
		return "", errors.New("it has a line break in it")
	}
	for _, part := range strings.Split(e, "/") {
		if part == ".." {
			return "", errors.New(`".." isn't allowed`)
		}
	}
	var p string
	switch {
	case e == "~":
		p = home
	case strings.HasPrefix(e, "~/"):
		p = filepath.Join(home, e[2:])
	case strings.HasPrefix(e, "~"):
		return "", errors.New("only your own home folder (~/) can be named")
	case filepath.IsAbs(e):
		p = filepath.Clean(e)
	default:
		p = filepath.Join(home, e)
	}
	if !within(p, home) {
		return "", fmt.Errorf("it is outside %s", home)
	}
	// Follow the part without wildcards as far as it exists; it must stay in the home folder.
	lit := p
	if i := strings.IndexAny(p, `*?[\`); i >= 0 {
		lit = filepath.Dir(p[:i+1])
	}
	for within(lit, home) {
		real, err := filepath.EvalSymlinks(lit)
		if err == nil {
			if !within(real, realHome) {
				return "", fmt.Errorf("it leads through a symlink to %s, outside %s", real, home)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		lit = filepath.Dir(lit)
	}
	return p, nil
}

// within reports whether path p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}
