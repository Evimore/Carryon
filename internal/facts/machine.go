package facts

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Fingerprint identifies the hardware across reinstalls and renames: the first 16 hex digits of
// sha256(product_uuid|board_serial|board_name) from /sys/class/dmi/id. It returns "" when any of the
// three can't be read (board_serial and product_uuid need root).
//
// It must stay identical to the prototype's machine_fingerprint, so "samehost" keeps matching the
// records the prototype wrote.
func (s *System) Fingerprint() string {
	parts := make([]string, 0, 3)
	for _, name := range []string{"product_uuid", "board_serial", "board_name"} {
		b, err := os.ReadFile(s.path("sys/class/dmi/id/" + name))
		if err != nil {
			return ""
		}
		parts = append(parts, pyStrip(pyText(b)))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:16]
}

// pyText mirrors Python's read_text(), which turns "\r\n" and "\r" into "\n".
func pyText(b []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\r", "\n")
}

// pyStrip mirrors Python's str.strip(): Go's whitespace plus the ASCII separators 0x1c-0x1f.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return (r >= 0x1c && r <= 0x1f) || unicode.IsSpace(r)
	})
}

// dmiChassis maps SMBIOS chassis types to hostnamectl's chassis names, as systemd-hostnamed does.
var dmiChassis = map[int]string{
	0x03: "desktop", 0x04: "desktop", 0x06: "desktop", 0x07: "desktop", 0x0d: "desktop",
	0x23: "desktop", 0x24: "desktop",
	0x08: "laptop", 0x09: "laptop", 0x0a: "laptop", 0x0e: "laptop",
	0x0b: "handset",
	0x11: "server", 0x17: "server", 0x1c: "server", 0x1d: "server",
	0x1e: "tablet",
	0x1f: "convertible", 0x20: "convertible",
	0x21: "embedded", 0x22: "embedded",
}

// acpiChassis maps the ACPI preferred power-management profile to chassis names, as
// systemd-hostnamed does when DMI says nothing useful.
var acpiChassis = map[int]string{
	1: "desktop", 3: "desktop", 6: "desktop",
	2: "laptop",
	4: "server", 5: "server", 7: "server",
	8: "tablet",
}

var chassisNames = map[string]bool{
	"desktop": true, "laptop": true, "convertible": true, "server": true, "tablet": true,
	"handset": true, "watch": true, "embedded": true, "vm": true, "container": true,
}

var machineInfoChassis = regexp.MustCompile(`(?m)^\s*CHASSIS=["']?([a-z]+)["']?\s*$`)

// Chassis returns the machine's chassis with hostnamectl's names (desktop, laptop, convertible,
// server, tablet, handset, watch, embedded, vm, container), or "" when unknown. It follows
// systemd-hostnamed's order: the CHASSIS= set in /etc/machine-info, a container, a virtual
// machine, the DMI chassis type, the ACPI power profile. When all of those say nothing, it asks
// hostnamectl.
func (s *System) Chassis(ctx context.Context) string {
	if b, err := os.ReadFile(s.path("etc/machine-info")); err == nil {
		if m := machineInfoChassis.FindStringSubmatch(string(b)); m != nil && chassisNames[m[1]] {
			return m[1]
		}
	}
	if _, err := os.Stat(s.path("run/systemd/container")); err == nil {
		return "container"
	}
	if s.hypervisor() {
		return "vm"
	}
	if n, err := strconv.ParseInt(s.readTrim("sys/class/dmi/id/chassis_type"), 0, 32); err == nil {
		if c := dmiChassis[int(n)]; c != "" {
			return c
		}
	}
	if n, err := strconv.Atoi(s.readTrim("sys/firmware/acpi/pm_profile")); err == nil {
		if c := acpiChassis[n]; c != "" {
			return c
		}
	}
	if c := strings.TrimSpace(s.output(ctx, "hostnamectl", "chassis")); chassisNames[c] {
		return c
	}
	return ""
}

// hypervisor reports whether the CPU says it runs under a hypervisor.
func (s *System) hypervisor() bool {
	f, err := os.Open(s.path("proc/cpuinfo"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(key) == "flags" {
			for _, flag := range strings.Fields(val) {
				if flag == "hypervisor" {
					return true
				}
			}
			return false // every CPU lists the same flags
		}
	}
	return false
}

// RootFS returns the type of the filesystem mounted on /, from /proc/self/mountinfo.
func (s *System) RootFS() string {
	b, err := os.ReadFile(s.path("proc/self/mountinfo"))
	if err != nil {
		return ""
	}
	fstype := ""
	for _, line := range strings.Split(string(b), "\n") {
		// id parent major:minor root mountpoint options [optional fields...] - fstype source super
		f := strings.Fields(line)
		if len(f) < 5 || f[4] != "/" {
			continue
		}
		for i := 6; i+1 < len(f); i++ {
			if f[i] == "-" {
				fstype = f[i+1] // the last mount on / is the one that's visible
				break
			}
		}
	}
	return fstype
}

var bootctlProduct = regexp.MustCompile(`Product:\s*(\S+)`)

var bootloaders = []string{"limine", "systemd-boot", "grub", "refind"}

// bootloaderFiles are config files that give a bootloader away, checked in this order.
var bootloaderFiles = []struct{ path, name string }{
	{"etc/default/limine", "limine"},
	{"etc/default/grub", "grub"},
	{"etc/sdboot-manage.conf", "systemd-boot"},
	{"boot/refind_linux.conf", "refind"},
}

// Bootloader returns limine, systemd-boot, grub, refind or "". It trusts the boot loader that
// `bootctl status` reports first, then looks for the bootloaders' config files.
func (s *System) Bootloader(ctx context.Context) string {
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if m := bootctlProduct.FindStringSubmatch(s.output(tctx, "bootctl", "status", "--no-pager")); m != nil {
		name := strings.ToLower(m[1])
		for _, b := range bootloaders {
			if strings.Contains(name, b) {
				return b
			}
		}
	}
	for _, f := range bootloaderFiles {
		if _, err := os.Stat(s.path(f.path)); err == nil {
			return f.name
		}
	}
	return ""
}
