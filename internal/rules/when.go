// Package rules decides which bundles start ticked on a machine (when-rules) and reads the user's
// bundle rules (bundles.conf). See docs/design.md, "Bundles and when-rules".
package rules

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Evimore/carryon/internal/record"
)

// Facts describe the machine a restore runs on.
type Facts struct {
	Host       string          // hostname
	Machine    string          // hardware fingerprint ("" when unknown)
	Chassis    string          // hostnamectl chassis: desktop, laptop...
	RootFS     string          // root filesystem type: btrfs, ext4...
	Bootloader string          // limine, systemd-boot, grub, refind or ""
	IDs        map[string]bool // hardware IDs, lower case: "pci:1234:abcd", "usb:...", "module:btusb", "bt:<mac>"
}

// ErrUnknownRule is returned for a when-rule Carryon doesn't know. The bundle is treated as
// "always", like the prototype did, so a typo never hides a bundle.
var ErrUnknownRule = errors.New("unknown when-rule")

// Evaluate reports whether a bundle with this when-rule starts ticked on the machine described by
// f, when restoring from the machine described by src. If not, reason says why. Rules joined with
// "+" must all hold.
func Evaluate(rule string, f Facts, src record.Meta) (ok bool, reason string, err error) {
	for _, part := range strings.Split(strings.TrimSpace(rule), "+") {
		ok, reason, e := evalOne(strings.TrimSpace(part), f, src)
		if e != nil && err == nil {
			err = e
		}
		if !ok {
			return false, reason, err
		}
	}
	return true, "", err
}

// Check reports whether a when-rule is one Carryon understands.
func Check(rule string) error {
	_, _, err := Evaluate(rule, Facts{}, record.Meta{})
	return err
}

func evalOne(rule string, f Facts, src record.Meta) (bool, string, error) {
	kind, arg, _ := strings.Cut(rule, ":")
	switch {
	case rule == "always":
		return true, "", nil
	case rule == "never":
		return false, "opt-in", nil
	case rule == "samehost":
		same := src.Host == f.Host
		if src.Machine != "" && f.Machine != "" {
			same = src.Machine == f.Machine // same hardware, whatever it's called now
		}
		return same, fmt.Sprintf("specific to %s (this is a different machine)", src.Name()), nil
	case kind == "chassis" && arg != "":
		return f.Chassis == arg, arg + "s only", nil
	case kind == "hw" && arg != "":
		var ids []string
		for _, x := range strings.Split(arg, ",") {
			if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
				ids = append(ids, x)
			}
		}
		for _, id := range ids {
			if f.IDs[id] {
				return true, "", nil
			}
		}
		var pretty []string
		for _, id := range ids[:min(2, len(ids))] {
			pretty = append(pretty, PrettyID(id))
		}
		return false, "needs " + strings.Join(pretty, " or ") + ", not found here", nil
	case kind == "rootfs" && arg != "":
		return f.RootFS == arg, fmt.Sprintf("needs a %s root filesystem (this one is %s)", arg, orUnknown(f.RootFS)), nil
	case kind == "bootloader" && arg != "":
		return f.Bootloader == arg, fmt.Sprintf("needs the %s bootloader (this machine uses %s)", arg, orUnknown(f.Bootloader)), nil
	}
	return true, "", fmt.Errorf("%w %q, treating it as 'always'", ErrUnknownRule, rule)
}

// PrettyID describes a hardware ID for people: "pci:1234:abcd" -> "PCI 1234:abcd".
func PrettyID(id string) string {
	kind, rest, _ := strings.Cut(id, ":")
	switch kind {
	case "pci":
		return "PCI " + rest
	case "usb":
		return "USB " + rest
	case "bt":
		return "Bluetooth adapter " + rest
	case "module":
		return "hardware driven by " + rest
	}
	return id
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
