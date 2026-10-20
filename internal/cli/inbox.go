package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

func newInboxCmd(dirs paths.Dirs) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Review suggestions about new things to back up",
		Long: `List the suggestions waiting for you: new folders Carryon found outside the backup, and
apps whose saved settings can be restored. Anyone can list them.

In a terminal, and as root, it then goes through them one by one: back it up, ignore it, or
decide later. Without a terminal, settle one with "sudo carryon add ID" or
"sudo carryon ignore ID".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			in, err := state.ReadInbox(dirs)
			if err != nil && !errors.Is(err, state.ErrInboxDamaged) {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				enc.SetEscapeHTML(false)
				return enc.Encode(in)
			}
			if err != nil {
				fmt.Fprintf(out, "note: %v; the next change sets it aside\n", err)
			}
			if len(in.Entries) == 0 {
				fmt.Fprintln(out, "Nothing is waiting in the inbox.")
				return nil
			}
			writeInbox(out, in.Entries)
			p := newPrompter(false)
			if _, plain := p.(ui.Defaults); plain {
				fmt.Fprintln(out, "\nSettle one with: sudo carryon add ID, or sudo carryon ignore ID")
				return nil
			}
			if geteuid() != 0 {
				fmt.Fprintln(out, "\nGo through them with: sudo carryon inbox")
				return nil
			}
			fmt.Fprintln(out)
			return reviewInbox(out, dirs, p, in.Entries)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// writeInbox lists entries, one or two lines each.
func writeInbox(out io.Writer, entries []state.Entry) {
	fmt.Fprintf(out, "%s waiting:\n\n", plural(len(entries), "suggestion"))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\n", e.ID, question(e))
		if d := details(e); d != "" {
			fmt.Fprintf(tw, "\t%s\n", d)
		}
	}
	tw.Flush()
}

// question is what an entry asks.
func question(e state.Entry) string {
	if e.Kind == state.KindRestore {
		return fmt.Sprintf("Restore the saved settings of %s?", e.Bundle)
	}
	return fmt.Sprintf("Back up %s?", e.Path)
}

// details says what Carryon knows about an entry: "1.2 GiB (large), probably jellyfin-server,
// found by the pacman hook on 2026-10-01".
func details(e state.Entry) string {
	var parts []string
	if e.Size > 0 {
		s := backup.HumanBytes(uint64(e.Size))
		if e.Large {
			s += " (large)"
		}
		parts = append(parts, s)
	} else if e.Large {
		parts = append(parts, "large")
	}
	if e.App != "" {
		parts = append(parts, "probably "+e.App)
	}
	if !e.Found.IsZero() || e.By != "" {
		found := "found"
		if e.By != "" {
			found += " by " + foundBy(e.By)
		}
		if !e.Found.IsZero() {
			found += " on " + e.Found.In(statusLoc).Format("2006-01-02")
		}
		parts = append(parts, found)
	}
	if e.Kind == state.KindRestore {
		parts = append(parts, "restore with: sudo carryon restore --bundle "+e.Bundle)
	}
	return strings.Join(parts, ", ")
}

// foundBy names what found an entry.
func foundBy(by string) string {
	switch by {
	case state.ByHook:
		return "the pacman hook"
	case state.ByBackup:
		return "the nightly backup"
	case state.ByFlatpak:
		return "the Flatpak watcher"
	}
	return by
}

// reviewInbox goes through the entries with the terminal UI. Leaving early keeps the rest.
func reviewInbox(out io.Writer, dirs paths.Dirs, p ui.Prompter, entries []state.Entry) error {
	for _, e := range entries {
		m := ui.Menu{Title: question(e), Text: details(e), Default: "later"}
		if e.Kind == state.KindRestore {
			m.Options = []ui.Option{
				{Value: "later", Label: "Later", Note: "keep the offer in the inbox"},
				{Value: "ignore", Label: "Don't offer it again"},
			}
		} else {
			m.Options = []ui.Option{
				{Value: "add", Label: "Back it up"},
				{Value: "ignore", Label: "Ignore", Note: "don't suggest it again"},
				{Value: "later", Label: "Later", Note: "keep it in the inbox"},
			}
		}
		choice, err := p.Menu(m)
		if errors.Is(err, ui.ErrCancelled) {
			fmt.Fprintln(out, "Stopped; the rest stays in the inbox.")
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "add":
			err = acceptPath(out, dirs, e.Path, e)
		case "ignore":
			err = ignoreEntry(out, dirs, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func newIgnoreCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "ignore PATH|ID",
		Short: "Stop suggesting a path",
		Long: `Stop suggesting a folder, or anything inside it. The folder is added to detect.ignore in
the config file, so the setting travels with the backup. It isn't left out of the backup: use
carryon exclude for that.

ID is a suggestion's ID from carryon inbox. Ignoring an offer to restore an app's saved settings
stops the offer.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := needRoot("carryon ignore"); err != nil {
				return err
			}
			e, found, err := inboxEntry(dirs, args[0])
			if err != nil {
				return err
			}
			if !found {
				p, err := filepath.Abs(args[0])
				if err != nil {
					return fmt.Errorf("can't make %q a full path: %w", args[0], err)
				}
				e = state.NewEntry(state.KindPath, p)
			}
			return ignoreEntry(cmd.OutOrStdout(), dirs, e)
		},
	}
}

// ignoreEntry stops suggesting what e is about: a path goes in detect.ignore, a restore offer's
// bundle in the inbox's ignored bundles. Either way the suggestions it covers are settled.
func ignoreEntry(out io.Writer, dirs paths.Dirs, e state.Entry) error {
	if e.Kind == state.KindRestore {
		if err := changeInbox(out, dirs, func(in *state.Inbox) []state.Entry {
			if !slices.Contains(in.IgnoredBundles, e.Bundle) {
				in.IgnoredBundles = append(in.IgnoredBundles, e.Bundle)
			}
			if gone, ok := in.Remove(e.ID); ok {
				return []state.Entry{gone}
			}
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(out, "Carryon won't offer to restore %s again\n", e.Bundle)
		return nil
	}
	p := e.Path
	var covered string
	_, err := config.Update(dirs.ConfigFile(), func(c *config.Config) error {
		for _, have := range c.Detect.Ignore {
			if under(p, filepath.Clean(have)) {
				covered = have
				return nil
			}
		}
		c.Detect.Ignore = append(slices.Clone(c.Detect.Ignore), p)
		return nil
	})
	if err != nil {
		return err
	}
	if covered != "" {
		fmt.Fprintf(out, "%s is already ignored (detect.ignore has %s)\n", p, covered)
	} else {
		fmt.Fprintf(out, "Added %s to detect.ignore in %s\n", p, dirs.ConfigFile())
	}
	return settlePaths(out, dirs, p)
}
