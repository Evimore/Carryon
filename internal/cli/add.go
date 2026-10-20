package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
)

func newAddCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "add PATH|ID",
		Short: "Back up a folder outside the home folders",
		Long: `Back up a system folder besides the built-in ones, such as /var/lib/jellyfin. The folder
is added to backup.extra_paths in the config file. Home folders are backed up through
backup.users and don't need this.

ID is a suggestion's ID from carryon inbox. Adding a suggested folder also puts it in its app's
bundle (a put line in bundles.conf, when the bundle wouldn't find it by name), and settles the
suggestion.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := needRoot("carryon add"); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			e, found, err := inboxEntry(dirs, args[0])
			if err != nil {
				return err
			}
			if found && e.Kind == state.KindRestore {
				return fmt.Errorf("%s offers to restore the saved settings of %s, not a folder to back up: restore them with \"sudo carryon restore --bundle %s\", or stop the offer with \"sudo carryon ignore %s\"", e.ID, e.Bundle, e.Bundle, e.ID)
			}
			p := e.Path
			if !found {
				if p, err = filepath.Abs(args[0]); err != nil {
					return fmt.Errorf("can't make %q a full path: %w", args[0], err)
				}
			}
			return acceptPath(out, dirs, p, e)
		},
	}
}

// inboxEntry finds the inbox entry arg names, by ID or by path. found is false when there is none;
// an arg shaped like an ID that the inbox doesn't hold is an error.
func inboxEntry(dirs paths.Dirs, arg string) (e state.Entry, found bool, err error) {
	in, err := state.ReadInbox(dirs)
	if err != nil && !errors.Is(err, state.ErrInboxDamaged) {
		return e, false, err
	}
	key := arg
	if !state.ValidEntryID(arg) {
		if key, err = filepath.Abs(arg); err != nil {
			return e, false, fmt.Errorf("can't make %q a full path: %w", arg, err)
		}
	}
	e, found = in.Find(key)
	if !found && state.ValidEntryID(arg) {
		if _, err := os.Lstat(arg); err != nil { // not a folder that happens to look like an ID
			return e, false, fmt.Errorf("there is no suggestion %s in the inbox: see the list with \"carryon inbox\"", arg)
		}
	}
	return e, found, nil
}

// acceptPath backs up folder p: it adds it to backup.extra_paths (and takes it out of
// detect.ignore), adds a put rule when e names a bundle that wouldn't find it by name, and
// settles the suggestions for p and for folders inside it. e is the zero Entry for a folder that
// wasn't suggested.
func acceptPath(out io.Writer, dirs paths.Dirs, p string, e state.Entry) error {
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
			if under(p, filepath.Clean(have)) {
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
	return settlePaths(out, dirs, p)
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
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

// settlePaths removes the path suggestions for folder p and for folders inside it.
func settlePaths(out io.Writer, dirs paths.Dirs, p string) error {
	return changeInbox(out, dirs, func(in *state.Inbox) []state.Entry {
		var gone []state.Entry
		for _, e := range slices.Clone(in.Entries) {
			if e.Kind == state.KindPath && under(e.Path, p) {
				in.Remove(e.ID)
				gone = append(gone, e)
			}
		}
		return gone
	})
}

// changeInbox applies change to the inbox when it would settle something, and says what it
// settled. change returns the entries it removed.
func changeInbox(out io.Writer, dirs paths.Dirs, change func(*state.Inbox) []state.Entry) error {
	in, err := state.ReadInbox(dirs)
	if err == nil && len(change(&in)) == 0 {
		return nil // nothing to settle: leave the inbox alone
	}
	var gone []state.Entry
	setAside, err := state.UpdateInbox(dirs, state.InboxWait, func(in *state.Inbox) error {
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
