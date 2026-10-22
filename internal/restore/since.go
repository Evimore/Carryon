package restore

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
)

// InstalledSince is what this machine has that its own backup doesn't. Restoring puts back what
// differs from the backup and never removes anything, so it's left as it is: the plan says so,
// with how to remove it by hand. Rolling the whole machine back is a job for snapper or
// Timeshift (docs/design.md, "Goals").
type InstalledSince struct {
	// Packages were installed on purpose here and aren't in the backup, sorted. Dependencies
	// aren't listed: removing these removes the ones only they needed.
	Packages []string
	// Remove is the command that removes Packages by hand, program first, such as pacman -Rns.
	Remove []string
	// Snapper says the root filesystem is btrfs with snapper set up for it, so snapper or
	// btrfs-assistant can roll a whole change back.
	Snapper bool
}

// snapperRootConfig is snapper's config for the root filesystem, there when snapper is set up
// for it.
const snapperRootConfig = "/etc/snapper/configs/root"

// installedSince works out what was installed here after the backup, when the plan restores the
// whole backup onto the machine it's of. It's nil otherwise, or when nothing was.
func (pl *Planner) installedSince(ctx context.Context) (*InstalledSince, error) {
	rec, here := pl.Source.Record, pl.Here
	if !pl.Everything || !sameMachine(rec.Meta, here.Facts) {
		return nil, nil
	}
	inRecord := map[string]bool{}
	for _, it := range rec.Items {
		if it.Type == record.TypePkg || it.Type == record.TypeAUR {
			inRecord[it.ID] = true
		}
	}
	if len(inRecord) == 0 {
		return nil, nil // a record without packages can't tell which ones are new
	}
	pkgs, err := here.Packages.Packages(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing what was installed since the backup: %w", err)
	}
	var names []string
	for _, p := range pkgs {
		if !p.AsDeps && !inRecord[p.Name] {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	slices.Sort(names)
	names = slices.Compact(names)
	return &InstalledSince{
		Packages: names,
		Remove:   here.Packages.RemoveCommand(names),
		Snapper:  here.Facts.RootFS == "btrfs" && here.lexists(snapperRootConfig),
	}, nil
}

// sameMachine reports whether here is the machine the record is of, as the samehost when-rule
// decides it: the same hardware, or the same hostname when the hardware isn't known. Facts that
// know neither never match.
func sameMachine(src record.Meta, here rules.Facts) bool {
	if here.Host == "" && here.Machine == "" {
		return false
	}
	same, _, _ := rules.Evaluate("samehost", here, src)
	return same
}

// sinceHeading heads the section in the plan.
const sinceHeading = "Installed since this backup (left as they are)"

// snapperHint says how to undo a whole change, when snapper is set up.
const snapperHint = "To undo a whole change, roll back to before it with snapper or btrfs-assistant."

// removeNote says nothing is removed, and the command that removes the packages by hand.
func (s *InstalledSince) removeNote(sep string) string {
	note := "Restoring never removes anything."
	if len(s.Remove) > 0 {
		note += " To remove these by hand:" + sep + "sudo " + strings.Join(s.Remove, " ")
	}
	return note
}

// summary writes the section in short, for the plan's summary.
func (s *InstalledSince) summary(w io.Writer) {
	fmt.Fprintf(w, "\n  %s: %s\n", sinceHeading, record.Brief(s.Packages, 8))
	fmt.Fprintf(w, "  %s\n", s.removeNote(" "))
	if s.Snapper {
		fmt.Fprintf(w, "  %s\n", snapperHint)
	}
}

// full writes the section with every package, for the plan file.
func (s *InstalledSince) full(w io.Writer) {
	fmt.Fprintf(w, "\n%s:\n", sinceHeading)
	for _, p := range s.Packages {
		fmt.Fprintf(w, "  %s\n", p)
	}
	fmt.Fprintf(w, "%s\n", s.removeNote("\n  "))
	if s.Snapper {
		fmt.Fprintln(w, snapperHint)
	}
}
