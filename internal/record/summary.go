package record

import (
	"fmt"
	"strings"
)

// SummaryFile is the readable summary of a record, written when it's published.
const SummaryFile = "SUMMARY.md"

// summaryItems is how many items SUMMARY.md lists per bundle.
const summaryItems = 30

// Summary renders SUMMARY.md: the machine, then each bundle with its first items. It ports the
// prototype's summary_md and gives the same text for a format 1 record.
func (r *Record) Summary() string {
	m := r.Meta
	byBundle := map[string][]Item{}
	for _, it := range r.Items {
		byBundle[it.Bundle] = append(byBundle[it.Bundle], it)
	}
	chassis := m.Chassis
	if chassis == "" {
		chassis = "unknown"
	}
	complete := "**Incomplete** (captured without root)."
	if m.Complete {
		complete = "Complete."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: system record\n\n", m.Name())
	fmt.Fprintf(&b, "%s, kernel %s, %s chassis. %s\n\n", usersLine(m), m.Kernel, chassis, complete)
	b.WriteString("Restore offers each bundle; its *when* rule decides whether it starts ticked on the target machine.\n\n")
	for _, bun := range r.Bundles {
		items := byBundle[bun.ID]
		fmt.Fprintf(&b, "## %s\n", bun.Title)
		fmt.Fprintf(&b, "`%s` · %d items · when: `%s`\n\n", bun.ID, len(items), bun.When)
		for i, it := range items {
			if i == summaryItems {
				fmt.Fprintf(&b, "- ... %d more\n", len(items)-summaryItems)
				break
			}
			fmt.Fprintf(&b, "- %s\n", it.Describe())
		}
		b.WriteString("\n")
	}
	b.WriteString("## Deliberately not in this record\n")
	b.WriteString("Secrets live only in the encrypted backup: Wi-Fi/VPN passwords, SSH host keys, /etc/shadow, " +
		"Bluetooth link keys, Tailscale state, /root and your home directory.\n")
	return b.String()
}

// usersLine is "User `alice`" for one user and "Users `alice`, `bob`" for several.
func usersLine(m Meta) string {
	users := m.AllUsers()
	if len(users) == 0 {
		return "User `" + m.User + "`"
	}
	names := make([]string, len(users))
	for i, u := range users {
		names[i] = "`" + u.Name + "`"
	}
	if len(names) == 1 {
		return "User " + names[0]
	}
	return "Users " + strings.Join(names, ", ")
}
