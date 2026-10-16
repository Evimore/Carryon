package facts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// HardwareIDs returns the IDs describing the machine's hardware, for "hw:" when-rules, all lower
// case: "pci:VVVV:DDDD", "usb:VVVV:PPPP", "bt:<adapter address>" and "module:NAME" for every loaded
// module and every module matching a device. Module names use "_", never "-".
func (s *System) HardwareIDs(ctx context.Context) (map[string]bool, error) {
	ids := map[string]bool{}
	for _, id := range s.PCIDevices() {
		ids[id] = true
	}
	for _, id := range s.USBDevices() {
		ids[id] = true
	}
	for _, mac := range s.BluetoothAdapters(ctx) {
		ids["bt:"+mac] = true
	}
	mods, err := s.Modules(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mods {
		ids["module:"+m] = true
	}
	return ids, nil
}

// PCIDevices returns the IDs of the PCI devices present, as "pci:VVVV:DDDD", sorted.
func (s *System) PCIDevices() []string {
	return s.busIDs("pci", "vendor", "device")
}

// USBDevices returns the IDs of the USB devices present, as "usb:VVVV:PPPP", sorted.
func (s *System) USBDevices() []string {
	return s.busIDs("usb", "idVendor", "idProduct")
}

func (s *System) busIDs(bus, vendor, product string) []string {
	devs, _ := filepath.Glob(s.path("sys/bus/" + bus + "/devices/*"))
	set := map[string]bool{}
	for _, dev := range devs {
		if id := readID(bus, dev, vendor, product); id != "" {
			set[id] = true
		}
	}
	return sorted(set)
}

// readID reads a device's vendor and product files into "bus:vvvv:pppp", or "" if it has none
// (USB interfaces, hubs' ports).
func readID(bus, dir, vendor, product string) string {
	v, err1 := os.ReadFile(filepath.Join(dir, vendor))
	p, err2 := os.ReadFile(filepath.Join(dir, product))
	if err1 != nil || err2 != nil {
		return ""
	}
	return strings.ToLower(bus + ":" + hexID(v) + ":" + hexID(p))
}

func hexID(b []byte) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(string(b))), "0x")
}

var bluetoothctlController = regexp.MustCompile(`(?m)^Controller ([0-9A-Fa-f:]{17})\b`)

// BluetoothAdapters returns the addresses of the Bluetooth adapters present, lower case, sorted.
// Current kernels don't show adapter addresses in sysfs, so this reads debugfs (needs root), and
// asks `bluetoothctl list` when that says nothing and an adapter is present.
func (s *System) BluetoothAdapters(ctx context.Context) []string {
	set := map[string]bool{}
	files, _ := filepath.Glob(s.path("sys/kernel/debug/bluetooth/hci*/identity"))
	for _, f := range files {
		// "<address> (type N) <irk> <rpa>"
		if b, err := os.ReadFile(f); err == nil {
			if addr, _, _ := strings.Cut(strings.TrimSpace(string(b)), " "); isMAC(addr) {
				set[strings.ToLower(addr)] = true
			}
		}
	}
	if len(set) == 0 {
		if hcis, _ := filepath.Glob(s.path("sys/class/bluetooth/hci*")); len(hcis) > 0 {
			tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			for _, m := range bluetoothctlController.FindAllStringSubmatch(s.output(tctx, "bluetoothctl", "list"), -1) {
				set[strings.ToLower(m[1])] = true
			}
		}
	}
	return sorted(set)
}

var macPattern = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

func isMAC(s string) bool { return macPattern.MatchString(s) && s != "00:00:00:00:00:00" }

// Modules returns the loaded kernel modules plus the modules matching each PCI and USB device's
// modalias (from `modprobe -R`), with "-" turned into "_", sorted. A kernel built without loadable
// modules has no /proc/modules; that isn't an error.
func (s *System) Modules(ctx context.Context) ([]string, error) {
	b, err := os.ReadFile(s.path("proc/modules"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading the loaded kernel modules: %w", err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			set[moduleName(f[0])] = true
		}
	}
	if s.Run != nil && s.Run.Exists("modprobe") {
		seen := map[string]bool{}
		for _, bus := range []string{"pci", "usb"} {
			files, _ := filepath.Glob(s.path("sys/bus/" + bus + "/devices/*/modalias"))
			for _, f := range files {
				alias := ""
				if b, err := os.ReadFile(f); err == nil {
					alias = strings.TrimSpace(string(b))
				}
				if alias == "" || seen[alias] {
					continue
				}
				seen[alias] = true
				for _, m := range strings.Fields(s.output(ctx, "modprobe", "-R", alias)) {
					set[moduleName(m)] = true
				}
			}
		}
	}
	return sorted(set), nil
}

// LoadedModules returns the names of the loaded kernel modules, as /proc/modules lists them, in
// its order. A kernel built without loadable modules has none.
func (s *System) LoadedModules() []string {
	b, _ := os.ReadFile(s.path("proc/modules"))
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

// ModuleExists reports whether a kernel module is installed for the running kernel, loaded or
// not (`modinfo -n` finds its file).
func (s *System) ModuleExists(ctx context.Context, mod string) bool {
	return strings.TrimSpace(s.output(ctx, "modinfo", "-n", mod)) != ""
}

// ModuleFirmware returns the firmware files a kernel module asks for (`modinfo -F firmware`),
// relative to the firmware folder and without a compression suffix.
func (s *System) ModuleFirmware(ctx context.Context, mod string) []string {
	return strings.Fields(s.output(ctx, "modinfo", "-F", "firmware", mod))
}

func moduleName(m string) string { return strings.ToLower(strings.ReplaceAll(m, "-", "_")) }

// DevicesForModule returns the PCI and USB devices a loaded module drives, as "pci:VVVV:DDDD" or
// "usb:VVVV:PPPP", sorted. For a USB interface, the device it belongs to is returned.
func (s *System) DevicesForModule(mod string) []string {
	set := map[string]bool{}
	drivers, _ := filepath.Glob(s.path(filepath.Join("sys/module", mod, "drivers", "*")))
	for _, drv := range drivers {
		bus, name, _ := strings.Cut(filepath.Base(drv), ":")
		vendor, product := "idVendor", "idProduct"
		if bus == "pci" {
			vendor, product = "vendor", "device"
		}
		entries, _ := filepath.Glob(s.path(filepath.Join("sys/bus", bus, "drivers", name, "*")))
		for _, e := range entries {
			real, err := filepath.EvalSymlinks(e)
			if err != nil {
				continue
			}
			for _, d := range []string{real, filepath.Dir(real)} {
				if id := readID(bus, d, vendor, product); id != "" {
					set[id] = true
					break
				}
			}
		}
	}
	return sorted(set)
}

// ModuleHolders returns the modules using mod, from /sys/module/<mod>/holders. A helper module
// (btmtk) drives no device itself; the modules holding it (btusb) do.
func (s *System) ModuleHolders(mod string) []string {
	entries, _ := os.ReadDir(s.path(filepath.Join("sys/module", mod, "holders")))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

var lspciSuffix = regexp.MustCompile(`\s*\[[0-9a-f]{4}\]$`)

// DeviceName returns a device's name for people, from `lspci -mm` or `lsusb`, or the ID itself
// when neither knows it.
func (s *System) DeviceName(ctx context.Context, id string) string {
	bus, vd, _ := strings.Cut(id, ":")
	switch bus {
	case "pci":
		line, _, _ := strings.Cut(s.output(ctx, "lspci", "-mm", "-d", vd), "\n")
		if f := splitQuoted(line); len(f) > 3 {
			return lspciSuffix.ReplaceAllString(f[3], "")
		}
	case "usb":
		if line := strings.TrimSpace(s.output(ctx, "lsusb", "-d", vd)); line != "" {
			if _, name, ok := strings.Cut(line, vd); ok {
				return strings.TrimSpace(name)
			}
			return line
		}
	}
	return id
}

// splitQuoted splits an `lspci -mm` line into fields: bare words and "double-quoted strings".
func splitQuoted(line string) []string {
	var out []string
	for line = strings.TrimSpace(line); line != ""; line = strings.TrimSpace(line) {
		if line[0] == '"' {
			end := strings.IndexByte(line[1:], '"')
			if end < 0 {
				return append(out, line[1:])
			}
			out = append(out, line[1:end+1])
			line = line[end+2:]
			continue
		}
		word, rest, _ := strings.Cut(line, " ")
		out = append(out, word)
		line = rest
	}
	return out
}

var (
	lsusbLine     = regexp.MustCompile(`ID ([0-9a-f]{4}):([0-9a-f]{4})\s*(.*)$`)
	fingerprintRx = regexp.MustCompile(`(?i)finger|fingprint|fprint`)
	hwdbUSB       = regexp.MustCompile(`(?i)usb:v([0-9a-f]{4})p([0-9a-f]{4})`)
)

// FingerprintReaders returns the USB IDs of the fingerprint readers present: devices named like
// one (by sysfs or lsusb), or listed in libfprint's or systemd's udev hwdb and rules. Sorted.
func (s *System) FingerprintReaders(ctx context.Context) []string {
	present := map[string]bool{}
	named := map[string]bool{}
	devs, _ := filepath.Glob(s.path("sys/bus/usb/devices/*"))
	for _, dev := range devs {
		id := readID("usb", dev, "idVendor", "idProduct")
		if id == "" {
			continue
		}
		present[id] = true
		var desc []string
		for _, f := range []string{"manufacturer", "product"} {
			if b, err := os.ReadFile(filepath.Join(dev, f)); err == nil {
				desc = append(desc, strings.TrimSpace(string(b)))
			}
		}
		if fingerprintRx.MatchString(strings.Join(desc, " ")) {
			named[id] = true
		}
	}
	for _, line := range strings.Split(s.output(ctx, "lsusb"), "\n") {
		if m := lsusbLine.FindStringSubmatch(line); m != nil && fingerprintRx.MatchString(m[3]) {
			named["usb:"+m[1]+":"+m[2]] = true
		}
	}
	out := map[string]bool{}
	for id := range named {
		if present[id] {
			out[id] = true
		}
	}
	var files []string
	for _, g := range []string{"usr/lib/udev/hwdb.d/*finger*", "usr/lib/udev/hwdb.d/*fprint*", "usr/lib/udev/rules.d/*fprint*"} {
		m, _ := filepath.Glob(s.path(g))
		files = append(files, m...)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range hwdbUSB.FindAllStringSubmatch(string(b), -1) {
			if id := strings.ToLower("usb:" + m[1] + ":" + m[2]); present[id] {
				out[id] = true
			}
		}
	}
	return sorted(out)
}

var (
	udevVendor  = regexp.MustCompile(`idVendor\}?==\s*"([0-9a-fA-F]{4})"`)
	udevProduct = regexp.MustCompile(`idProduct\}?==\s*"([0-9a-fA-F]{4})"`)
)

// AppDevices returns the present USB devices an app's udev rules are written for, as
// "usb:VVVV:PPPP", sorted. rules holds the contents of the app's udev rules files; present holds
// hardware IDs like those from HardwareIDs (anything but "usb:" IDs is ignored).
//
// Only apps made for a few device makers say something about a machine (OpenLinkHub: Corsair), so
// when the rules name more than 3 makers (OpenRGB: dozens) it returns nothing.
func AppDevices(rules []string, present []string) []string {
	pairs, vendors := map[string]bool{}, map[string]bool{}
	for _, text := range rules {
		for _, line := range strings.Split(text, "\n") {
			v := udevVendor.FindStringSubmatch(line)
			p := udevProduct.FindStringSubmatch(line)
			switch {
			case v != nil && p != nil:
				pairs[strings.ToLower(v[1]+":"+p[1])] = true
			case v != nil:
				vendors[strings.ToLower(v[1])] = true
			}
		}
	}
	makers := map[string]bool{}
	for p := range pairs {
		makers[p[:4]] = true
	}
	for v := range vendors {
		makers[v] = true
	}
	if len(makers) == 0 || len(makers) > 3 {
		return nil
	}
	out := map[string]bool{}
	for _, id := range present {
		id = strings.ToLower(id)
		vp, ok := strings.CutPrefix(id, "usb:")
		if ok && (pairs[vp] || vendors[strings.SplitN(vp, ":", 2)[0]]) {
			out[id] = true
		}
	}
	return sorted(out)
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
