package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/detect"
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

// acceptPath backs up folder p, as detect.Accept does; see there.
func acceptPath(out io.Writer, dirs paths.Dirs, p string, e state.Entry) error {
	return detect.Accept(out, dirs, p, e, state.InboxWait)
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool { return detect.Under(p, dir) }

// settlePaths removes the path suggestions for folder p and for folders inside it.
func settlePaths(out io.Writer, dirs paths.Dirs, p string) error {
	return detect.SettlePaths(out, dirs, p, state.InboxWait)
}

// changeInbox applies change to the inbox when it would settle something; see detect.ChangeInbox.
func changeInbox(out io.Writer, dirs paths.Dirs, change func(*state.Inbox) []state.Entry) error {
	return detect.ChangeInbox(out, dirs, state.InboxWait, change)
}
