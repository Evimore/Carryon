package detect

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
)

// Accept backs up folder p, as `carryon add` does and as detect.new_paths = "auto" does by
// itself: it adds p to backup.extra_paths (and takes it out of detect.ignore), adds a put rule when
// e names a bundle that wouldn't find it by name, and settles the suggestions for p and for folders
// inside it. e is the zero Entry for a folder that wasn't suggested. wait is how long it waits for
// the inbox lock. What it did is written to out.
func Accept(out io.Writer, dirs paths.Dirs, p string, e state.Entry, wait time.Duration) error {
	p = filepath.Clean(p)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "note: %s doesn't exist yet; it is backed up once it does\n", p)
	}
	var covered string
	unignored := false
	_, err := config.Update(dirs.ConfigFile(), func(c *config.Config) error {
		if i := slices.Index(c.Detect.Ignore, p); i >= 0 {
			c.Detect.Ignore = slices.Delete(slices.Clone(c.Detect.Ignore), i, i+1)
			unignored = true
		}
		for _, have := range c.Backup.ExtraPaths {
			if Under(p, filepath.Clean(have)) {
				covered = have
				return nil
			}
		}
		c.Backup.ExtraPaths = append(slices.Clone(c.Backup.ExtraPaths), p)
		return nil
	})
	if err != nil {
		return err
	}
	if covered != "" {
		fmt.Fprintf(out, "%s is already backed up (backup.extra_paths has %s)\n", p, covered)
	} else {
		fmt.Fprintf(out, "Added %s to backup.extra_paths in %s\n", p, dirs.ConfigFile())
	}
	if unignored {
		fmt.Fprintf(out, "Took %s out of detect.ignore\n", p)
	}
	if e.Bundle != "" && !(e.Bundle == "app-"+e.App && bundle.FindsByName(p, e.App)) {
		if err := addPutRule(out, dirs.BundlesFile(), e.Bundle, p); err != nil {
			return err
		}
	}
	return SettlePaths(out, dirs, p, wait)
}

// Under reports whether p is dir or inside it.
func Under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// underAny reports whether p is one of dirs or inside one.
func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if Under(p, filepath.Clean(d)) {
			return true
		}
	}
	return false
}

// addPutRule appends "put <bundle> path <folder>" to bundles.conf, so the folder joins that bundle
// in the record, unless the file has that rule already.
func addPutRule(out io.Writer, file, bundleID, folder string) error {
	if strings.ContainsAny(bundleID+folder, " \t\r\n") {
		fmt.Fprintf(out, "note: %s can't be written as a bundles.conf rule (it has spaces), so it stays in its own bundle\n", folder)
		return nil
	}
	rule := fmt.Sprintf("put %s path %s", bundleID, globQuote(folder))
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Join(strings.Fields(line), " ") == rule {
			return nil
		}
	}
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		data = append(data, '\n')
	}
	data = append(data, rule+"\n"...)
	if err := config.WriteFileAtomic(file, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Added \"%s\" to %s, so it travels with %s\n", rule, file, bundleID)
	return nil
}

// globQuote makes the glob characters in a path match only themselves.
func globQuote(p string) string {
	return strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(p)
}

// SettlePaths removes the path suggestions for folder p and for folders inside it, waiting for the
// inbox lock no longer than wait.
func SettlePaths(out io.Writer, dirs paths.Dirs, p string, wait time.Duration) error {
	return ChangeInbox(out, dirs, wait, func(in *state.Inbox) []state.Entry {
		var gone []state.Entry
		for _, e := range slices.Clone(in.Entries) {
			if e.Kind == state.KindPath && Under(e.Path, p) {
				in.Remove(e.ID)
				gone = append(gone, e)
			}
		}
		return gone
	})
}

// ChangeInbox applies change to the inbox when it would settle something, and says what it
// settled. change returns the entries it removed.
func ChangeInbox(out io.Writer, dirs paths.Dirs, wait time.Duration, change func(*state.Inbox) []state.Entry) error {
	in, err := state.ReadInbox(dirs)
	if err == nil && len(change(&in)) == 0 {
		return nil // nothing to settle: leave the inbox alone
	}
	var gone []state.Entry
	setAside, err := state.UpdateInbox(dirs, wait, func(in *state.Inbox) error {
		gone = change(in)
		return nil
	})
	if setAside != "" {
		fmt.Fprintf(out, "note: the inbox was damaged; it was set aside as %s\n", setAside)
	}
	if err != nil {
		return fmt.Errorf("the suggestion is still in the inbox (%w): run the same command again to settle it", err)
	}
	for _, e := range gone {
		fmt.Fprintf(out, "Settled %s from the inbox\n", e.ID)
	}
	return nil
}
