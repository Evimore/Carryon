package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
)

func newBundlesCmd(dirs paths.Dirs) *cobra.Command {
	var dir string
	var all bool
	cmd := &cobra.Command{
		Use:   "bundles",
		Short: "Show how a system record is grouped for restore",
		Long: `Show the bundles in a system record and the items in each.

--record can point at any record folder, such as one written by carryon capture --out DIR.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := record.Load(dir)
			if err != nil {
				return fmt.Errorf("can't read the system record: %w", err)
			}
			out := cmd.OutOrStdout()
			byBundle := map[string][]record.Item{}
			for _, it := range r.Items {
				byBundle[it.Bundle] = append(byBundle[it.Bundle], it)
			}
			fmt.Fprintf(out, "System record of %s (%d items in %d bundles, format %d)\n",
				r.Meta.Name(), len(r.Items), len(r.Bundles), r.Meta.Format)
			for _, b := range r.Bundles {
				items := byBundle[b.ID]
				fmt.Fprintf(out, "\n%s   %s · when %s\n", b.Title, b.ID, b.When)
				shown := items
				if !all && len(shown) > 6 {
					shown = shown[:6]
				}
				for _, it := range shown {
					fmt.Fprintf(out, "      %s\n", it.Describe())
				}
				if len(items) > len(shown) {
					fmt.Fprintf(out, "      ... %d more (--all)\n", len(items)-len(shown))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "record", dirs.RecordDir(), "record directory to read")
	cmd.Flags().BoolVar(&all, "all", false, "list every item")
	return cmd
}
