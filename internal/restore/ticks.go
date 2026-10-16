package restore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/ui"
)

// Tick is a bundle with whether it starts ticked on this machine, and why.
type Tick struct {
	Bundle record.Bundle
	Items  []record.Item // the bundle's items, in record order
	On     bool
	Reason string // why it starts unticked, or what made a conditional bundle tick
}

// DefaultTicks evaluates each bundle's when-rule against this machine's facts, in restore order.
// Unknown when-rules count as "always", like the prototype; they come back as warnings.
func DefaultTicks(rec *record.Record, here rules.Facts) (ticks []Tick, warnings []string) {
	byBundle := map[string][]record.Item{}
	for _, it := range rec.Items {
		byBundle[it.Bundle] = append(byBundle[it.Bundle], it)
	}
	for _, b := range rec.Bundles {
		on, reason, err := rules.Evaluate(b.When, here, rec.Meta)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("bundle %s: %v", b.ID, err))
		}
		if on && strings.Contains(b.When, "samehost") {
			reason = "same machine"
		}
		ticks = append(ticks, Tick{Bundle: b, Items: byBundle[b.ID], On: on, Reason: reason})
	}
	return ticks, warnings
}

// Choice key of an item: its record key, plus the user for per-user items, since two users can
// each have the same group or app.
func choiceKey(it record.Item) string {
	if it.User != "" {
		return it.Key() + "@" + it.User
	}
	return it.Key()
}

// Choices are the bundles and items picked for a restore from one machine, remembered so the next
// restore from it can start from them. The JSON is the prototype's selections file.
type Choices struct {
	Date       string   `json:"date"`       // when they were made, RFC 3339
	Target     string   `json:"target"`     // the machine restored onto
	Bundles    []string `json:"bundles"`    // ticked bundles, sorted
	Deselected []string `json:"deselected"` // unticked items' keys, sorted
}

// choicesFile is where the choices for restoring from a machine are kept.
func choicesFile(stateDir, machine string) string {
	return filepath.Join(stateDir, "selections", strings.ReplaceAll(machine, "/", "_")+".json")
}

// LoadChoices reads the choices last made for restoring from machine. It returns nil when there
// are none or they can't be read.
func LoadChoices(stateDir, machine string) *Choices {
	data, err := os.ReadFile(choicesFile(stateDir, machine))
	if err != nil {
		return nil
	}
	var c Choices
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

// SaveChoices remembers the choices for restoring from machine, in Carryon's state folder.
func SaveChoices(stateDir, machine string, c Choices) error {
	slices.Sort(c.Bundles)
	slices.Sort(c.Deselected)
	if c.Bundles == nil {
		c.Bundles = []string{}
	}
	if c.Deselected == nil {
		c.Deselected = []string{}
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := choicesFile(stateDir, machine)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("remembering your choices: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("remembering your choices: %w", err)
	}
	return nil
}

// Picked is what the user chose to restore.
type Picked struct {
	Bundles    []string // ticked bundles, in restore order
	Deselected []string // keys of unticked items inside ticked bundles
}

// Has reports whether an item is picked: its bundle is ticked and the item isn't unticked.
func (p Picked) Has(it record.Item) bool {
	return slices.Contains(p.Bundles, it.Bundle) && !slices.Contains(p.Deselected, choiceKey(it))
}

// Select returns the record's picked items, in record order.
func (p Picked) Select(items []record.Item) []record.Item {
	var out []record.Item
	for _, it := range items {
		if p.Has(it) {
			out = append(out, it)
		}
	}
	return out
}

// Choices turns the pick into choices to remember.
func (p Picked) Choices(target string, now time.Time) Choices {
	return Choices{Date: now.Format(time.RFC3339), Target: target,
		Bundles: slices.Clone(p.Bundles), Deselected: slices.Clone(p.Deselected)}
}

// DefaultPick takes the default ticks, as restore --defaults does.
func DefaultPick(ticks []Tick) Picked {
	var p Picked
	for _, t := range ticks {
		if t.On {
			p.Bundles = append(p.Bundles, t.Bundle.ID)
		}
	}
	return p
}

// Pick asks the user which bundles, and which items inside them, to restore. When choices from
// an earlier restore from this machine exist, it offers to start from them. Ported from pick: the
// prototype's second "fine-tune" list is the checklist's own item view here.
func Pick(ask ui.Prompter, ticks []Tick, prev *Choices, from, onto string) (Picked, error) {
	usePrev := false
	if prev != nil {
		date := prev.Date
		if len(date) >= 10 {
			date = date[:10]
		}
		var err error
		usePrev, err = ask.Confirm(ui.Confirm{Title: fmt.Sprintf("Start from the choices you made on %s?", date), Default: true})
		if err != nil {
			return Picked{}, err
		}
	}
	c := ui.Checklist{
		Title: fmt.Sprintf("Restore from %s onto %s", from, onto),
		Text:  "Unticked bundles are skipped entirely. Open a bundle to untick single items. Nothing changes until you confirm the plan.",
		Flag:  "--defaults",
	}
	for _, t := range ticks {
		g := ui.Group{ID: t.Bundle.ID, Title: t.Bundle.Title, Summary: countItems(len(t.Items)), On: t.On}
		if !t.On || t.Reason == "same machine" {
			g.Reason = t.Reason
		}
		if usePrev {
			g.On = slices.Contains(prev.Bundles, t.Bundle.ID)
		}
		if len(t.Items) > 1 {
			for _, it := range t.Items {
				k := choiceKey(it)
				g.Items = append(g.Items, ui.Item{ID: k, Label: describe(it),
					On: !(usePrev && slices.Contains(prev.Deselected, k))})
			}
		}
		c.Groups = append(c.Groups, g)
	}
	sel, err := ask.Checklist(c)
	if err != nil {
		if errors.Is(err, ui.ErrCancelled) {
			return Picked{}, errors.New("restore cancelled: nothing was changed")
		}
		return Picked{}, err
	}
	var p Picked
	for _, t := range ticks { // restore order, whatever order the selection lists them in
		if !sel.HasGroup(t.Bundle.ID) {
			continue
		}
		p.Bundles = append(p.Bundles, t.Bundle.ID)
		for _, it := range t.Items {
			if k := choiceKey(it); slices.Contains(sel.Unticked, k) && !slices.Contains(p.Deselected, k) {
				p.Deselected = append(p.Deselected, k)
			}
		}
	}
	return p, nil
}

func countItems(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}

// describe is an item's description, with whose it is for per-user items.
func describe(it record.Item) string {
	if it.User != "" {
		return it.Describe() + "  (" + it.User + ")"
	}
	return it.Describe()
}
