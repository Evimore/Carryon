package why

import (
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/catalog"
)

// WriteText writes the answer for people.
func WriteText(w io.Writer, a Answer) error {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	switch {
	case a.BackedUp:
		line("%s is backed up.", a.Path)
	case a.Folder == nil && a.Package != nil:
		line("%s isn't in the backup itself, but it comes back with its package.", a.Path)
	default:
		line("%s is not backed up.", a.Path)
	}
	if !a.Exists {
		line("It doesn't exist here; this is what happens to it once it does.")
	}

	if a.Folder != nil {
		line("It's in %s.", folderText(*a.Folder))
		a.writeLeftOut(line)
	} else {
		switch {
		case a.OtherUser != "":
			line("It's in %s's home folder, but backup.users leaves %s out.", a.OtherUser, a.OtherUser)
			line("To back it up, add %s to backup.users.", a.OtherUser)
		case a.Package == nil:
			line("It isn't in any backed-up folder.")
		}
	}
	if a.Package != nil {
		a.writePackage(line)
	}
	switch {
	case a.Waiting != nil:
		what := "it"
		if a.Waiting.Path != a.Path {
			what = a.Waiting.Path
		}
		line("Carryon suggests backing up %s: it's waiting in carryon inbox as %s.", what, a.Waiting.ID)
		line("To back it up: sudo carryon add %s", a.Waiting.ID)
	case a.Ignored != "":
		line("detect.ignore has %s, so Carryon doesn't suggest backing it up.", a.Ignored)
		if a.Folder == nil && a.Package == nil && a.OtherUser == "" {
			line("To back it up anyway: sudo carryon add %s", a.Path)
		}
	case a.Folder == nil && a.Package == nil && a.OtherUser == "":
		line("To back it up: sudo carryon add %s", a.Path)
	}

	switch {
	case !a.RecordRead && a.RecordError != "":
		line("")
		line("The system record couldn't be read (%s), so Carryon can't say which item and bundle hold it.", a.RecordError)
	case !a.RecordRead:
		line("")
		line("Without root, Carryon can't read the system record, so it can't say which item and bundle hold it")
		line("or which other system folders the record backs up. For the full answer: sudo carryon why %s", a.Path)
	case len(a.Items) > 0:
		line("")
		line("In the system record:")
		for _, it := range a.Items {
			who := ""
			if it.User != "" {
				who = " (" + it.User + ")"
			}
			line("  %s%s", it.describe, who)
			switch it.Regrouped {
			case "":
				line("    bundle %s, %q, because %s.", it.Bundle, it.Title, it.Reason.Text)
			case "-":
				line("    bundle %s, %q. bundles.conf now drops it, so the next capture leaves it out of the record.", it.Bundle, it.Title)
			default:
				line("    bundle %s, %q. Grouped again now, it goes to %s, because %s; the next capture moves it.",
					it.Bundle, it.Title, it.Regrouped, it.Reason.Text)
			}
			switch {
			case it.Ticked == nil:
				line("    when-rule: %s", it.When)
			case *it.Ticked:
				line("    when-rule: %s, so it starts ticked when restoring here.", it.When)
			default:
				line("    when-rule: %s, so it starts unticked here: %s.", it.When, it.Unticked)
			}
		}
	default:
		line("")
		line("No item in the system record holds it.")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeLeftOut says what leaves a path in a backed-up folder out, or takes it back in.
func (a Answer) writeLeftOut(line func(string, ...any)) {
	if t := a.TakenBack; t != nil {
		switch {
		case t.Helps:
			line("The pattern %s, from %s, would leave it out, but %s, from %s, takes it back.", t.Over, t.OverFrom, t.Pattern, t.From)
		case a.Excluded != nil && a.Excluded.At != a.Path:
			line("%s, from %s, matches it, but it can't bring back something inside a folder that is left out.", t.Pattern, t.From)
		case a.Excluded != nil:
			line("%s, from %s, takes it back from %s, but a later pattern leaves it out again.", t.Pattern, t.From, t.Over)
		}
	}
	if e := a.Excluded; e != nil {
		what := "it"
		if e.At != a.Path {
			what = e.At
		}
		line("The pattern %s, from %s, leaves %s out.", e.Pattern, e.From, what)
		switch e.Kind {
		case catalog.FromDefaults, catalog.FromCatalog:
			line("To back it up anyway, add !%s to excludes.txt, or switch the pattern off in backup.skip_default_excludes.",
				catalog.QuotePath(e.At))
		case catalog.FromUser:
			line("To back it up, take the pattern out of %s.", e.From)
		case catalog.FromAutomatic:
			if strings.Contains(e.From, "backup's own folder") {
				line("A backup never copies the backup into itself.")
			} else {
				line("Mounts inside home folders, like cloud folders and network shares, are left out: what lives there is kept elsewhere.")
			}
		}
	}
	if r := a.Redownload; r != nil {
		line("Left out on purpose: %s. %s.", r.Label, capitalize(r.How))
	}
	if a.CacheDir != "" {
		line("%s holds a CACHEDIR.TAG file, which marks it as a cache, and backups leave caches out.", a.CacheDir)
		line("To back it up, delete that file.")
	}
	if l := a.LeftOut; l != nil {
		if strings.Contains(l.Why, "extra_paths") {
			short := *l
			short.Why = ""
			line("%s.", capitalize(short.String()))
			line("To include it: sudo carryon add %s", l.Path)
		} else {
			line("%s.", capitalize(l.String()))
		}
	}
	if a.Subvolume != "" {
		line("It's on the btrfs subvolume %s, which is backed up as a folder of its own.", a.Subvolume)
	}
}

// writePackage says how a package file comes back.
func (a Answer) writePackage(line func(string, ...any)) {
	p := a.Package
	switch {
	case !a.RecordRead && a.Folder != nil:
		line("It belongs to the package %s: if you haven't changed it, a restore leaves it to the package.", p.Name)
	case !a.RecordRead:
		line("It belongs to the package %s, which brings it back when it's reinstalled, if you haven't changed it.", p.Name)
	case a.Folder != nil:
		line("It belongs to the package %s and the record has no changes to it, so a restore leaves it to the package.", p.Name)
	case p.InRecord:
		line("It belongs to the package %s, which the record lists, so reinstalling the package brings it back.", p.Name)
		line("The record has no changes to it.")
	default:
		line("It belongs to the package %s, but the record doesn't list that package.", p.Name)
	}
}

// folderText describes a backed-up folder.
func folderText(t backup.Target) string {
	switch t.Kind {
	case backup.TargetSystem:
		return t.Path + ", which is always backed up"
	case backup.TargetRecord:
		return t.Path + ", which the system record backs up"
	case backup.TargetExtra:
		return t.Path + ", which backup.extra_paths adds to the backup"
	case backup.TargetHome:
		return t.User + "'s home folder, " + t.Path
	}
	return t.Path
}

func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
