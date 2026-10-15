package capture

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
)

var (
	cargoLine  = regexp.MustCompile(`^(\S+) v\S+`)
	npmModules = regexp.MustCompile(`^.*/node_modules/`)
)

// captureTools records what language package managers installed: global npm packages and
// system-wide Flatpak apps, then each user's cargo and pipx installs and their own Flatpak apps.
// Ported from capture_tools. A tool that isn't installed or fails adds nothing.
func (c *capturer) captureTools(ctx context.Context) {
	r := c.opt.Runner
	if r.Exists("npm") {
		out, _ := r.Output(ctx, "npm", "ls", "-g", "--depth=0", "--parseable")
		lines := nonEmptyLines(out)
		if len(lines) > 0 {
			lines = lines[1:] // the global folder itself
		}
		for _, line := range lines {
			if name := npmModules.ReplaceAllString(line, ""); name != "npm" && name != "corepack" {
				c.add(record.Item{Type: record.TypeNpm, ID: name})
			}
		}
	}
	if r.Exists("flatpak") {
		c.flatpaks(ctx, r, "--system", "system", "")
	}
	for _, u := range c.res.Users {
		ur := c.userRunner(u)
		if c.userHas(r, u, ".cargo/bin/cargo", "cargo") {
			out, _ := ur.Output(ctx, "cargo", "install", "--list")
			for _, line := range nonEmptyLines(out) {
				if m := cargoLine.FindStringSubmatch(line); m != nil {
					c.add(record.Item{Type: record.TypeCargo, ID: m[1], User: u.Name})
				}
			}
		}
		if c.userHas(r, u, ".local/bin/pipx", "pipx") {
			out, _ := ur.Output(ctx, "pipx", "list", "--short")
			for _, line := range nonEmptyLines(out) {
				c.add(record.Item{Type: record.TypePipx, ID: strings.Fields(line)[0], User: u.Name})
			}
		}
		if r.Exists("flatpak") {
			c.flatpaks(ctx, ur, "--user", "user", u.Name)
		}
	}
}

// flatpaks records the Flatpak apps of one installation (--system or --user).
func (c *capturer) flatpaks(ctx context.Context, r sysexec.Runner, which, scope, user string) {
	out, _ := r.Output(ctx, "flatpak", "list", "--app", which, "--columns=application,origin")
	for _, line := range nonEmptyLines(out) {
		f := strings.Split(line, "\t")
		if len(f) >= 2 {
			c.add(record.Item{Type: record.TypeFlatpak, ID: f[0], Origin: f[1], Scope: scope, User: user})
		}
	}
}

// userHas reports whether a program is on root's PATH or in the user's own bin folder (where
// cargo and pipx put themselves).
func (c *capturer) userHas(r sysexec.Runner, u User, inHome, name string) bool {
	if r.Exists(name) {
		return true
	}
	fi, err := os.Stat(c.path(filepath.Join(u.Home, inHome)))
	return err == nil && fi.Mode().IsRegular()
}

func nonEmptyLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, "\r"))
		}
	}
	return out
}
