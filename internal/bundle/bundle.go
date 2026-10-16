// Package bundle groups a system record's items into bundles, the units a restore offers to tick,
// and decides each bundle's title and when-rule. See docs/design.md, "Bundles and when-rules".
//
// Grouping goes in this order of precedence: the user's own rules (bundles.conf), app bundles named
// after hand-made units, extra repositories and AUR packages, well-known places, a package's edited
// configs with its service, settings folders and autostart entries named like a package, hardware,
// then the built-in catch-all bundles. A unit's needs go wherever the unit went.
//
// On a machine with several users, bundles hold every user's items and each item keeps its user:
// one app bundle holds alice's and bob's settings for that app.
//
// Machine answers the few questions grouping asks the system (kernel modules, devices, which
// package owns a program), so tests can fake it.
//
// It ports the prototype's assign_bundles and its helpers (legacy/sysbackup.py).
package bundle

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
)

// Machine answers what grouping needs to know about the machine the record was captured on.
// Errors are treated as "don't know", like the prototype did.
type Machine interface {
	// ReadFile reads a system file by its absolute path.
	ReadFile(path string) ([]byte, error)
	// LookPath finds a program in the system's PATH, or returns "".
	LookPath(prog string) string
	// Owners maps the given paths to the packages owning them; unowned paths are left out.
	Owners(ctx context.Context, paths []string) map[string]string
	// FilesOf lists the files the given packages own.
	FilesOf(ctx context.Context, pkgs []string) []string
	// ModuleExists reports whether a kernel module is installed.
	ModuleExists(ctx context.Context, mod string) bool
	// LoadedModules lists the loaded kernel modules.
	LoadedModules() []string
	// ModuleFirmware lists the firmware files a module asks for (relative, uncompressed names).
	ModuleFirmware(ctx context.Context, mod string) []string
	// ModuleHolders lists the modules using a module.
	ModuleHolders(mod string) []string
	// DevicesForModule lists the devices a module drives, as "pci:VVVV:DDDD" or "usb:VVVV:PPPP".
	DevicesForModule(mod string) []string
	// DeviceName is a device's name for people, or the ID when unknown.
	DeviceName(ctx context.Context, id string) string
	// USBDevices lists the USB devices present, as "usb:VVVV:PPPP".
	USBDevices() []string
}

// Input is what grouping needs: the captured items and what capture learned on the way.
type Input struct {
	Items []record.Item
	// Rules are the user's bundles.conf rules. The zero value means none.
	Rules rules.BundleRules
	// Host is the machine's hostname, shown in the identity bundle's title.
	Host string
	// RepoOf maps each package the configured repositories offer to its repository.
	RepoOf map[string]string
	// FingerprintReaders are the fingerprint readers present ("usb:VVVV:PPPP").
	FingerprintReaders []string
	// BluetoothMACs are the adapters with pairings, lower case.
	BluetoothMACs []string
	// RecordDir is the record folder holding copies of system files under files/, for the PAM
	// check. Empty skips it.
	RecordDir string
	// Machine answers questions about devices, modules and programs. Nil knows nothing: no
	// hardware bundles get device when-rules and autostart entries aren't traced to packages.
	Machine Machine
}

// Assign groups the items. It returns the items the user's drop rules keep, each with its bundle
// set, and the bundles in restore order. in.Items isn't changed.
func Assign(ctx context.Context, in Input) ([]record.Item, record.Bundles) {
	g := &grouper{ctx: ctx, in: in, m: in.Machine, titles: map[string]string{}, whens: map[string]string{}}
	if g.m == nil {
		g.m = noMachine{}
	}
	for _, it := range in.Items {
		if dropped(it, in.Rules.Drops) {
			continue
		}
		it.Bundle = ""
		g.items = append(g.items, it)
	}
	g.diskLayout()
	g.userRules()
	g.appStems()
	g.pamFingerprint()
	g.wellKnown()
	g.editedConfigs()
	g.settingsFolders()
	g.extraFolders()
	g.hardware()
	g.fallbacks()
	g.unitNeeds()
	g.appDevices()
	g.titles["identity"] = fmt.Sprintf("Machine identity: hostname %s, SSH host keys, Tailscale node", in.Host)
	return g.items, g.bundles()
}

type grouper struct {
	ctx    context.Context
	in     Input
	m      Machine
	items  []record.Item
	titles map[string]string // titles found while grouping
	whens  map[string]string // when-rules found while grouping
}

func dropped(it record.Item, drops []rules.Drop) bool {
	for _, d := range drops {
		if d.Match.Matches(it) {
			return true
		}
	}
	return false
}

// free reports whether item i has no bundle yet.
func (g *grouper) free(i int) bool { return g.items[i].Bundle == "" }

// Files with disk UUIDs always go to disk-layout, whose title says they're shown as a diff and
// never copied. The prototype let an app bundle or the user's rules take them first
// (/etc/default/limine joined a "limine" app bundle).
func (g *grouper) diskLayout() {
	for i, it := range g.items {
		if it.Type == record.TypeFile && ReviewOnly.MatchString(it.ID) {
			g.items[i].Bundle = "disk-layout"
		}
	}
}

// 1. The user's own rules: the first matching put wins.
func (g *grouper) userRules() {
	for i, it := range g.items {
		if !g.free(i) {
			continue
		}
		for _, p := range g.in.Rules.Puts {
			if p.Match.Matches(it) {
				g.items[i].Bundle = p.Bundle
				break
			}
		}
	}
}

// 2. App bundles: whatever shares a name with a hand-made unit, an extra repository or an AUR
// package. A bundle named after an AUR package alone needs at least two items.
func (g *grouper) appStems() {
	anchors := map[string]bool{}
	stems := map[string]bool{}
	for _, it := range g.items {
		switch {
		case it.Type == record.TypeSvc && it.Pkg == "",
			it.Type == record.TypeRepo,
			it.Type == record.TypeUunit && !strings.Contains(it.ID, "/"):
			anchors[stemOf(it.ID)] = true
			stems[stemOf(it.ID)] = true
		case it.Type == record.TypeAUR:
			stems[stemOf(it.ID)] = true
		}
	}
	for s := range stems {
		if runes(s) < 4 {
			delete(stems, s)
		}
	}
	for i, it := range g.items {
		if !g.free(i) {
			continue
		}
		best := ""
		for s := range stems {
			if stemMatches(it, s, g.in.RepoOf) && (runes(s) > runes(best) || runes(s) == runes(best) && s < best) {
				best = s
			}
		}
		g.items[i].Bundle = best
	}
	counts := map[string]int{}
	for _, it := range g.items {
		counts[it.Bundle]++
	}
	for i, it := range g.items {
		if stems[it.Bundle] && !anchors[it.Bundle] && counts[it.Bundle] < 2 {
			g.items[i].Bundle = ""
		}
	}
}

// PAM files using the fingerprint reader (sudo, polkit...) go with the reader.
func (g *grouper) pamFingerprint() {
	if g.in.RecordDir == "" {
		return
	}
	for i, it := range g.items {
		if !g.free(i) || it.Type != record.TypeFile || !strings.HasPrefix(it.ID, "/etc/pam.d/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(g.in.RecordDir, "files", strings.TrimPrefix(it.ID, "/")))
		if err == nil && strings.Contains(string(b), "pam_fprintd") {
			g.items[i].Bundle = "fingerprint"
		}
	}
}

// 3. Well-known places.
func (g *grouper) wellKnown() {
	for i, it := range g.items {
		if !g.free(i) {
			continue
		}
		for _, r := range wellKnownRules {
			if r.matches(it) {
				g.items[i].Bundle = r.bundle
				break
			}
		}
	}
	if len(g.in.FingerprintReaders) > 0 {
		g.whens["fingerprint"] = "hw:" + strings.Join(g.in.FingerprintReaders, ",")
	}
	var xkb []string
	for _, it := range g.items {
		if it.Bundle == "xkb" && strings.Contains(it.ID, "/symbols/") {
			xkb = append(xkb, path.Base(it.ID))
		}
	}
	if len(xkb) > 0 {
		sort.Strings(xkb)
		g.titles["xkb"] = "Keyboard tweaks (/etc/xkb): " + strings.Join(xkb, ", ")
	}
}

// 4. A package's edited configs, with its service and files named after it, when there are at
// least two.
func (g *grouper) editedConfigs() {
	var pkgs []string
	groups := map[string][]int{}
	for i, it := range g.items {
		if g.free(i) && it.Type == record.TypeFile && it.Kind == record.KindModified && it.Pkg != "" {
			if groups[it.Pkg] == nil {
				pkgs = append(pkgs, it.Pkg)
			}
			groups[it.Pkg] = append(groups[it.Pkg], i)
		}
	}
	for _, pkg := range pkgs {
		its := groups[pkg]
		in := map[int]bool{}
		for _, i := range its {
			in[i] = true
		}
		var extra []int
		service := false
		for i, it := range g.items {
			if !g.free(i) || in[i] {
				continue
			}
			isSvc := it.Type == record.TypeSvc && it.Pkg == pkg
			if isSvc || it.Type == record.TypeFile && it.Kind == record.KindUnowned && namedAfter(it.ID, pkg) {
				extra = append(extra, i)
				service = service || isSvc
			}
		}
		if len(its)+len(extra) < 2 {
			continue
		}
		b := "cfg-" + pkg
		for _, i := range append(its, extra...) {
			g.items[i].Bundle = b
		}
		g.titles[b] = pkg + " settings"
		if service {
			g.titles[b] += " + service"
		}
	}
}

// namedAfter reports whether name holds pkg as a word: at the start or after "/", "_" or "-", and
// at the end or before "/", ".", "_" or "-" (the prototype's (^|[/_-])pkg([/._-]|$)).
func namedAfter(name, pkg string) bool {
	if pkg == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(name[i:], pkg)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(pkg)
		if (start == 0 || strings.IndexByte("/_-", name[start-1]) >= 0) &&
			(end == len(name) || strings.IndexByte("/._-", name[end]) >= 0) {
			return true
		}
		i = start + 1
	}
}

// explicit maps app keys to the packages installed on purpose, by index; the last one wins.
func (g *grouper) explicit() (byKey, byName map[string]int) {
	byKey, byName = map[string]int{}, map[string]int{}
	for i, it := range g.items {
		if (it.Type == record.TypePkg || it.Type == record.TypeAUR) && !it.AsDeps {
			byKey[appKey(it.ID)] = i
		}
	}
	for _, i := range byKey {
		byName[g.items[i].ID] = i
	}
	return byKey, byName
}

// joinApp puts item i into the app bundle of package p, making it if needed.
func (g *grouper) joinApp(i, p int, title string) {
	if g.free(p) {
		g.items[p].Bundle = "app-" + g.items[p].ID
		g.titles[g.items[p].Bundle] = title
	}
	g.items[i].Bundle = g.items[p].Bundle
}

// 4b. Apps: a settings folder named like an installed package (~/.config/OpenRGB <-> openrgb) forms
// a bundle with that package, and so does an autostart entry named like it or launching its
// program.
func (g *grouper) settingsFolders() {
	byKey, byName := g.explicit()
	for _, autostart := range []bool{false, true} {
		for i, it := range g.items {
			rel := it.Home
			if !g.free(i) || it.Type != record.TypePath || rel == "" || strings.HasPrefix(rel, ".config/autostart/") != autostart {
				continue
			}
			if !autostart && !strings.HasPrefix(rel, ".config/") && !strings.HasPrefix(rel, ".local/share/") {
				continue
			}
			name := path.Base(rel)
			if autostart {
				name = stem(rel)
			}
			k := appKey(name)
			p, ok := -1, false
			if runes(k) >= 3 {
				p, ok = byKey[k]
			}
			if !ok && autostart { // e.g. com.example.App.desktop -> its package
				p, ok = byName[autostartOwner(g.ctx, g.m, it.ID)]
			}
			if !ok {
				continue
			}
			title := path.Base(rel) + " (app + settings)"
			if autostart {
				title = g.items[p].ID + " (app + autostart)"
			}
			g.joinApp(i, p, title)
		}
	}
}

// Added system folders named like a package (/var/lib/foo <-> foo) join that package.
func (g *grouper) extraFolders() {
	byKey, _ := g.explicit()
	for i, it := range g.items {
		if !g.free(i) || !it.Extra {
			continue
		}
		if p, ok := byKey[appKey(path.Base(it.ID))]; ok {
			g.joinApp(i, p, path.Base(it.ID)+" (app + data)")
		}
	}
}

const firmwareDir = "/usr/lib/firmware/"

// 5. Hardware: kernel parameters for a driver or for hiding devices, and firmware files.
func (g *grouper) hardware() {
	var fwBundles []string
	fw := map[string][]string{}
	for i, it := range g.items {
		if !g.free(i) {
			continue
		}
		switch {
		case it.Type == record.TypeKparam:
			k, v, _ := strings.Cut(it.ID, "=")
			mod := ""
			if strings.Contains(k, ".") {
				mod, _, _ = strings.Cut(k, ".")
			}
			if (k == "pci-stub.ids" || k == "vfio-pci.ids" || k == "vfio_pci.ids") && v != "" {
				b := "kparam-" + k
				g.items[i].Bundle = b
				var ids []string
				for _, x := range strings.Split(v, ",") {
					ids = append(ids, "pci:"+strings.ToLower(x))
				}
				g.whens[b] = "hw:" + strings.Join(ids, ",")
				g.titles[b] = fmt.Sprintf("Kernel param %s (hides %s)", it.ID, g.m.DeviceName(g.ctx, ids[0]))
			} else if mod != "" && g.m.ModuleExists(g.ctx, mod) {
				b := "kparam-" + mod
				g.items[i].Bundle = b
				g.whens[b] = "hw:module:" + strings.ReplaceAll(mod, "-", "_")
				g.titles[b] = "Kernel params for the " + mod + " driver"
			}
		case it.Type == record.TypeFile && strings.HasPrefix(it.ID, firmwareDir):
			d := strings.TrimPrefix(path.Dir(it.ID)+"/", firmwareDir)
			b := "firmware-" + strings.ReplaceAll(strings.TrimSuffix(d, "/"), "/", "-")
			if d == "" { // a file right in the firmware folder
				b = "firmware-files"
			}
			g.items[i].Bundle = b
			if fw[b] == nil {
				fwBundles = append(fwBundles, b)
			}
			fw[b] = append(fw[b], it.ID)
		}
	}
	for _, b := range fwBundles {
		when, devs := g.firmwareWhen(fw[b])
		g.whens[b] = when
		g.titles[b] = "Firmware " + strings.TrimPrefix(b, "firmware-")
		if len(devs) > 0 {
			g.titles[b] += " for " + g.m.DeviceName(g.ctx, devs[0])
		}
	}
	if len(g.in.BluetoothMACs) > 0 {
		var ids []string
		for _, m := range g.in.BluetoothMACs {
			ids = append(ids, "bt:"+m)
		}
		g.whens["bluetooth"] = "hw:" + strings.Join(ids, ",")
	}
}

// firmwareWhen is the when-rule of a firmware bundle: the devices driven by the loaded modules
// asking for these files, or "samehost" when none are known. A generic driver bound to more than
// 3 devices says nothing about the machine. devs are the devices, sorted.
func (g *grouper) firmwareWhen(paths []string) (when string, devs []string) {
	wanted := map[string]bool{}
	for _, p := range paths {
		rel := strings.TrimPrefix(p, firmwareDir)
		rel = strings.TrimSuffix(strings.TrimSuffix(rel, ".zst"), ".xz")
		wanted[rel] = true
	}
	set := map[string]bool{}
	seen := map[string]bool{}
	for _, mod := range g.m.LoadedModules() {
		if seen[mod] {
			continue
		}
		seen[mod] = true
		uses := false
		for _, f := range g.m.ModuleFirmware(g.ctx, mod) {
			uses = uses || wanted[f]
		}
		if !uses {
			continue
		}
		// helper modules (btmtk) drive no device themselves; the modules using them (btusb) do
		found := map[string]bool{}
		for _, u := range append([]string{mod}, g.m.ModuleHolders(mod)...) {
			for _, d := range g.m.DevicesForModule(u) {
				found[d] = true
			}
		}
		if len(found) <= 3 {
			for d := range found {
				set[d] = true
			}
		}
	}
	if len(set) == 0 {
		return "samehost", nil
	}
	for d := range set {
		devs = append(devs, d)
	}
	sort.Strings(devs)
	var named []string
	for _, d := range devs {
		if strings.HasPrefix(d, "pci:") || strings.HasPrefix(d, "usb:") {
			named = append(named, d)
		}
	}
	return "hw:" + strings.Join(devs, ","), named
}

// 6. Everything else goes into the built-in catch-all bundles, except a unit's needs.
func (g *grouper) fallbacks() {
	for i, it := range g.items {
		if !g.free(i) || it.ForUnit != "" {
			continue
		}
		switch it.Type {
		case record.TypeFile:
			// edited package files didn't exist in the prototype; they're tweaks of a package too
			if it.Kind == record.KindModified || it.Kind == record.KindEdited {
				g.items[i].Bundle = "configs"
			} else {
				g.items[i].Bundle = "other-files"
			}
		case record.TypePath:
			if it.Home != "" {
				g.items[i].Bundle = "home-apps"
			} else {
				g.items[i].Bundle = "other-paths"
			}
		default:
			g.items[i].Bundle = fallbackOf(it.Type)
		}
	}
}

// 7. A unit's needs go wherever the unit went: the same user's unit first, then any.
func (g *grouper) unitNeeds() {
	type unit struct{ user, id string }
	byUser, byID := map[unit]string{}, map[string]string{}
	for _, it := range g.items {
		if (it.Type == record.TypeSvc || it.Type == record.TypeUunit) && it.Bundle != "" {
			byUser[unit{it.User, it.ID}] = it.Bundle
			if _, ok := byID[it.ID]; !ok {
				byID[it.ID] = it.Bundle
			}
		}
	}
	for i, it := range g.items {
		if !g.free(i) {
			continue
		}
		b, ok := byUser[unit{it.User, it.ForUnit}]
		if !ok {
			b, ok = byID[it.ForUnit]
		}
		if !ok {
			b = fallbackOf(it.Type)
		}
		g.items[i].Bundle = b
	}
}

// 8. Apps made for specific USB devices (OpenLinkHub: Corsair) start ticked where one is plugged in.
func (g *grouper) appDevices() {
	var present []string
	asked := false
	for _, b := range g.bundleIDs() {
		if _, ok := Builtin[b]; ok {
			continue
		}
		if _, ok := g.in.Rules.Whens[b]; ok {
			continue
		}
		if _, ok := g.whens[b]; ok {
			continue
		}
		var pkgs []string
		for _, it := range g.items {
			if it.Bundle == b && (it.Type == record.TypePkg || it.Type == record.TypeAUR) && !it.AsDeps {
				pkgs = append(pkgs, it.ID)
			}
		}
		if len(pkgs) == 0 {
			continue
		}
		var texts []string
		for _, f := range g.m.FilesOf(g.ctx, pkgs) {
			if !udevRule.MatchString(f) {
				continue
			}
			if b, err := g.m.ReadFile(f); err == nil {
				texts = append(texts, string(b))
			}
		}
		if len(texts) == 0 {
			continue
		}
		if !asked {
			present, asked = g.m.USBDevices(), true
		}
		if devs := facts.AppDevices(texts, present); len(devs) > 0 {
			g.whens[b] = "hw:" + strings.Join(devs, ",")
		}
	}
}

var udevRule = regexp.MustCompile(`/udev/rules\.d/[^/]+\.rules$`)

// bundleIDs lists the bundles in use, sorted.
func (g *grouper) bundleIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range g.items {
		if !seen[it.Bundle] {
			seen[it.Bundle] = true
			out = append(out, it.Bundle)
		}
	}
	sort.Strings(out)
	return out
}

// bundles describes every bundle in use, in restore order. The user's titles and when-rules come
// first, then those found while grouping, then the built-in ones.
func (g *grouper) bundles() record.Bundles {
	var out record.Bundles
	for _, id := range g.bundleIDs() {
		info := Builtin[id]
		title := first(g.in.Rules.Titles[id], g.titles[id], info.Title, capitalize(id)+" (auto-grouped)")
		when := first(g.in.Rules.Whens[id], g.whens[id], info.When, "always")
		out = append(out, record.Bundle{ID: id, Title: title, When: when})
	}
	sort.SliceStable(out, func(i, j int) bool { return less(out[i].ID, out[j].ID) })
	return out
}

func first(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// less orders bundles by rank, then ID.
func less(a, b string) bool {
	if ra, rb := Rank(a), Rank(b); ra != rb {
		return ra < rb
	}
	return a < b
}

// SortItems sorts items like the record lists them: by bundle in restore order, then type, id
// and user.
func SortItems(items []record.Item) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Bundle != b.Bundle {
			return less(a.Bundle, b.Bundle)
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.User < b.User
	})
}

// noMachine knows nothing about the machine.
type noMachine struct{}

func (noMachine) ReadFile(string) ([]byte, error)                    { return nil, os.ErrNotExist }
func (noMachine) LookPath(string) string                             { return "" }
func (noMachine) Owners(context.Context, []string) map[string]string { return nil }
func (noMachine) FilesOf(context.Context, []string) []string         { return nil }
func (noMachine) ModuleExists(context.Context, string) bool          { return false }
func (noMachine) LoadedModules() []string                            { return nil }
func (noMachine) ModuleFirmware(context.Context, string) []string    { return nil }
func (noMachine) ModuleHolders(string) []string                      { return nil }
func (noMachine) DevicesForModule(string) []string                   { return nil }
func (noMachine) DeviceName(_ context.Context, id string) string     { return id }
func (noMachine) USBDevices() []string                               { return nil }
