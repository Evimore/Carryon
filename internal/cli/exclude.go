package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

func newExcludeCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "exclude PATH",
		Short: "Leave a path out of the backup",
		Long: `Leave a path out of the backup by adding a restic pattern to excludes.txt.

PATH is a full path (/var/lib/docker), a path that exists here (relative ones are made
full), or a restic pattern such as "/home/*/.cache" or "node_modules". A pattern without a
leading / leaves out matching names anywhere.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := needRoot("carryon exclude"); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			pattern := strings.TrimSpace(args[0])
			if pattern != "" && !filepath.IsAbs(pattern) {
				if _, err := os.Lstat(pattern); err == nil {
					if abs, err := filepath.Abs(pattern); err == nil {
						pattern = abs
					}
				}
			}
			defaults, err := config.ReadExcludes(dirs.DefaultExcludes())
			if err != nil {
				return err
			}
			if slices.Contains(defaults, pattern) {
				fmt.Fprintf(out, "%s is already left out by the default excludes (%s)\n", pattern, dirs.DefaultExcludes())
				return nil
			}
			file := dirs.ExcludesFile()
			added, err := config.AddExclude(file, dirs.DefaultExcludes(), pattern)
			if err != nil {
				return err
			}
			if !added {
				fmt.Fprintf(out, "%s is already in %s\n", pattern, file)
				return nil
			}
			fmt.Fprintf(out, "Added %s to %s\n", pattern, file)
			if !strings.HasPrefix(pattern, "/") {
				fmt.Fprintln(out, "It has no leading /, so it leaves out matching names anywhere.")
			}
			return nil
		},
	}
}
