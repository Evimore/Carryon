package capture

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/record"
)

// secretPaths are system folders restored from the encrypted backup rather than the record:
// wifi passwords, Bluetooth pairings, the Tailscale node, enrolled fingerprints and root's home.
var secretPaths = []string{
	"/etc/NetworkManager/system-connections", "/var/lib/bluetooth", "/var/lib/tailscale",
	"/var/lib/fprint", "/root",
}

// SSHHostKeys is the path item for the machine's SSH host keys.
const SSHHostKeys = "/etc/ssh/ssh_host_*"

// homeSubdirs are folders in a home whose entries are recorded one by one, so apps can be
// restored one at a time. Autostart entries are one per app.
var homeSubdirs = []string{".config", ".local", ".local/share", ".config/autostart"}

var btMAC = regexp.MustCompile(`^[0-9A-Fa-f:]{17}$`)

// sysPathItem is a system path restored from the backup, with its owner (service users can get
// other numbers on another machine). Ported from sys_path_item.
func (c *capturer) sysPathItem(abs string, extra bool) record.Item {
	it := record.Item{Type: record.TypePath, ID: abs, Extra: extra}
	full := c.path(abs)
	if fi, err := os.Stat(full); err == nil {
		uid, gid := c.owner(full, fi)
		it.Owner = c.ids.owner(uid, gid)
		it.UID, it.GID = &uid, &gid
	}
	return it
}

// capturePaths records what is restored straight from the encrypted backup: secrets, /root, /srv,
// the configured extra paths, and for each user every top-level entry of their home and every
// entry of ~/.config, ~/.local and ~/.local/share. Ported from capture_paths.
func (c *capturer) capturePaths() error {
	covered := map[string]bool{}
	for _, p := range secretPaths {
		if _, err := os.Lstat(c.path(p)); err == nil {
			c.add(c.sysPathItem(p, false))
			covered[p] = true
		}
	}
	if keys, _ := filepath.Glob(c.path("/etc/ssh/ssh_host_*")); len(keys) > 0 {
		c.add(record.Item{Type: record.TypePath, ID: SSHHostKeys})
	}
	if entries, err := os.ReadDir(c.path("/srv")); err == nil && len(entries) > 0 {
		c.add(c.sysPathItem("/srv", false))
		covered["/srv"] = true
	}
	for _, p := range c.opt.Config.Backup.ExtraPaths {
		p = filepath.Clean(p)
		// /etc, /usr and /opt are recorded file by file; homes have their own items
		if covered[p] || p == "/etc" || p == "/usr/local" || underAny(p, []string{"/etc", "/usr", "/opt"}) || c.inHome(p) {
			continue
		}
		if _, err := os.Lstat(c.path(p)); err != nil {
			continue
		}
		covered[p] = true
		c.add(c.sysPathItem(p, true))
	}
	if entries, err := os.ReadDir(c.path("/var/lib/bluetooth")); err == nil {
		for _, e := range entries {
			if btMAC.MatchString(e.Name()) {
				c.res.BluetoothMACs = append(c.res.BluetoothMACs, strings.ToLower(e.Name()))
			}
		}
	}

	m, err := catalog.NewMatcher(c.opt.Excludes)
	if err != nil {
		return err
	}
	mounts := map[string]bool{}
	for _, mt := range c.mounts {
		mounts[filepath.Clean(mt.MountPoint)] = true
	}
	for _, u := range c.res.Users {
		home := filepath.Clean(u.Home)
		var cand []string
		top, err := os.ReadDir(c.path(home))
		if err != nil {
			c.warn("can't read %s's home folder %s: %v", u.Name, home, err)
			continue
		}
		for _, e := range top {
			if e.Name() != ".config" && e.Name() != ".local" {
				cand = append(cand, filepath.Join(home, e.Name()))
			}
		}
		for _, sub := range homeSubdirs {
			d := filepath.Join(home, sub)
			// Lstat: a settings folder that is a symlink leads elsewhere; it's backed up as a link
			if fi, err := os.Lstat(c.path(d)); err != nil || !fi.IsDir() {
				continue
			}
			entries, _ := os.ReadDir(c.path(d))
			for _, e := range entries {
				if (sub == ".local" && e.Name() == "share") || (sub == ".config" && e.Name() == "autostart") {
					continue
				}
				cand = append(cand, filepath.Join(d, e.Name()))
			}
		}
		sort.Strings(cand)
		for _, p := range cand {
			// user units travel in their own bundles, so unticking one really leaves it out
			if mounts[p] || m.Excluded(p) || p == filepath.Join(home, ".config/systemd") {
				continue
			}
			rel, _ := filepath.Rel(home, p)
			c.add(record.Item{Type: record.TypePath, ID: p, Home: rel, User: u.Name})
		}
	}
	return nil
}

// inHome reports whether path is a captured user's home or inside it.
func (c *capturer) inHome(path string) bool {
	for _, u := range c.res.Users {
		h := filepath.Clean(u.Home)
		if path == h || strings.HasPrefix(path, h+"/") {
			return true
		}
	}
	return false
}
