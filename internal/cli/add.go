package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

func newAddCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "add PATH",
		Short: "Back up a folder outside the home folders",
		Long: `Back up a system folder besides the built-in ones, such as /var/lib/jellyfin. The folder
is added to backup.extra_paths in the config file. Home folders are backed up through
backup.users and don't need this.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := needRoot("carryon add"); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			p, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("can't make %q a full path: %w", args[0], err)
			}
			if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(out, "note: %s doesn't exist yet; it is backed up once it does\n", p)
			}
			var covered string
			_, err = config.Update(dirs.ConfigFile(), func(c *config.Config) error {
				for _, have := range c.Backup.ExtraPaths {
					h := filepath.Clean(have)
					if p == h || strings.HasPrefix(p, strings.TrimSuffix(h, "/")+"/") {
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
				return nil
			}
			fmt.Fprintf(out, "Added %s to backup.extra_paths in %s\n", p, dirs.ConfigFile())
			return nil
		},
	}
}
