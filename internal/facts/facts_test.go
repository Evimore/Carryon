package facts

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
)

var ctx = context.Background()

func desktopRunner() *fakeRunner {
	return &fakeRunner{
		have: map[string]bool{"modprobe": true},
		out: map[string]string{
			"modprobe -R pci:v00001234d0000ABCDsv00001234sd00000001bc03sc00i00": "example-gpu\n",
			"modprobe -R pci:v00001234d00001111sv00001234sd00000001bc0Csc03i30": "xhci_pci\n",
			"modprobe -R usb:v0A0Bp0C0Dd0100dcE0dsc01dp01icE0isc01ip01in00":     "btusb\n",
		},
	}
}

func TestCollect(t *testing.T) {
	root := buildTree(t, "desktop.txt")
	got, err := Collect(ctx, root, desktopRunner())
	if err != nil {
		t.Fatal(err)
	}
	want := rules.Facts{
		Host:       "desktop",
		Machine:    "463d3149799255c3",
		Chassis:    "desktop",
		RootFS:     "btrfs",
		Bootloader: "limine",
		IDs: map[string]bool{
			"pci:1234:abcd": true, "pci:1234:1111": true,
			"usb:1d6b:0002": true, "usb:0a0b:0c0d": true, "usb:0f0f:0101": true,
			"usb:0e0e:0202": true, "usb:0c0c:1b2c": true,
			"bt:00:11:22:aa:bb:cc": true,
			"module:btusb":         true, "module:btmtk": true, "module:snd_hda_intel": true,
			"module:example_gpu": true, "module:xhci_pci": true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect:\n got %+v\nwant %+v", got, want)
	}

	// The facts feed when-rules.
	for rule, ok := range map[string]bool{
		"hw:module:btusb": true, "hw:pci:1234:ABCD": true, "hw:bt:00:11:22:aa:bb:cc": true,
		"hw:usb:9999:9999": false, "chassis:desktop+rootfs:btrfs+bootloader:limine": true,
	} {
		if got, _, err := rules.Evaluate(rule, want, record.Meta{Host: "desktop"}); err != nil || got != ok {
			t.Errorf("Evaluate(%q) = %v, %v; want %v", rule, got, err, ok)
		}
	}
}

func TestCollectWithoutPrograms(t *testing.T) {
	root := buildTree(t, "desktop.txt")
	got, err := Collect(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.IDs["module:example_gpu"] || !got.IDs["module:btusb"] || got.Bootloader != "limine" {
		t.Errorf("without a runner, only files are read: got %+v", got)
	}
}

func TestCollectNotASystem(t *testing.T) {
	if _, err := Collect(ctx, t.TempDir(), nil); err == nil {
		t.Error("an empty folder gave no error")
	}
}

func TestFingerprint(t *testing.T) {
	root := buildTree(t, "desktop.txt")
	// sha256("00000000-1111-2222-3333-444444444444|EXAMPLE-SERIAL-0001|Example Board X1"),
	// computed with the prototype's machine_fingerprint.
	const want = "463d3149799255c3"
	if got := New(root, nil).Fingerprint(); got != want {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}

	// Python's strip() also drops \x1c-\x1f, and read_text() turns \r\n into \n.
	write(t, root, "sys/class/dmi/id/board_serial", "\x1f\tEXAMPLE-SERIAL-0001\r\n")
	if got := New(root, nil).Fingerprint(); got != want {
		t.Errorf("Fingerprint with odd whitespace = %q, want %q", got, want)
	}

	if err := os.Remove(filepath.Join(root, "sys/class/dmi/id/board_serial")); err != nil {
		t.Fatal(err)
	}
	if got := New(root, nil).Fingerprint(); got != "" {
		t.Errorf("Fingerprint without board_serial = %q, want \"\"", got)
	}
}

func TestChassis(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		run   map[string]string
		want  string
	}{
		{"dmi desktop", map[string]string{"sys/class/dmi/id/chassis_type": "3\n"}, nil, "desktop"},
		{"dmi laptop", map[string]string{"sys/class/dmi/id/chassis_type": "10\n"}, nil, "laptop"},
		{"dmi convertible", map[string]string{"sys/class/dmi/id/chassis_type": "31\n"}, nil, "convertible"},
		{"dmi server", map[string]string{"sys/class/dmi/id/chassis_type": "23\n"}, nil, "server"},
		{"machine-info wins", map[string]string{
			"etc/machine-info":              "PRETTY_HOSTNAME=Desktop\nCHASSIS=tablet\n",
			"sys/class/dmi/id/chassis_type": "3\n",
		}, nil, "tablet"},
		{"container", map[string]string{"run/systemd/container": "docker\n", "sys/class/dmi/id/chassis_type": "3\n"}, nil, "container"},
		{"vm", map[string]string{
			"proc/cpuinfo":                  "processor\t: 0\nflags\t\t: fpu hypervisor lm\n",
			"sys/class/dmi/id/chassis_type": "1\n",
		}, nil, "vm"},
		{"acpi profile", map[string]string{"sys/class/dmi/id/chassis_type": "1\n", "sys/firmware/acpi/pm_profile": "2\n"}, nil, "laptop"},
		{"hostnamectl", map[string]string{"sys/class/dmi/id/chassis_type": "2\n"},
			map[string]string{"hostnamectl chassis": "handset\n"}, "handset"},
		{"hostnamectl nonsense", nil, map[string]string{"hostnamectl chassis": "Unknown command\n"}, ""},
		{"nothing", nil, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for p, c := range tt.files {
				write(t, root, p, c)
			}
			if got := New(root, &fakeRunner{out: tt.run}).Chassis(ctx); got != tt.want {
				t.Errorf("Chassis = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRootFS(t *testing.T) {
	tests := []struct{ name, mountinfo, want string }{
		{"plain", "1 0 8:1 / / rw - ext4 /dev/sda1 rw\n", "ext4"},
		{"optional fields", "1 0 8:1 / / rw shared:1 master:2 - xfs /dev/sda1 rw\n", "xfs"},
		{"last mount on / wins", "1 0 8:1 / / rw - ext4 /dev/sda1 rw\n2 1 0:30 / / rw - overlay overlay rw\n", "overlay"},
		{"no root", "1 0 8:1 / /home rw - ext4 /dev/sda1 rw\n", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "proc/self/mountinfo", tt.mountinfo)
			if got := New(root, nil).RootFS(); got != tt.want {
				t.Errorf("RootFS = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBootloader(t *testing.T) {
	tests := []struct {
		name    string
		bootctl string
		files   []string
		want    string
	}{
		{"bootctl systemd-boot", "System:\n     Firmware: UEFI 2.70\nCurrent Boot Loader:\n      Product: systemd-boot 256.1-1-arch\n", []string{"etc/default/grub"}, "systemd-boot"},
		{"bootctl limine", "Current Boot Loader:\n      Product: Limine 9.2.0\n", nil, "limine"},
		{"bootctl unknown product", "Current Boot Loader:\n      Product: ExampleBoot 1.0\n", []string{"etc/default/grub"}, "grub"},
		{"limine before grub", "", []string{"etc/default/grub", "etc/default/limine"}, "limine"},
		{"sdboot-manage", "", []string{"etc/sdboot-manage.conf"}, "systemd-boot"},
		{"refind", "", []string{"boot/refind_linux.conf"}, "refind"},
		{"nothing", "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tt.files {
				write(t, root, f, "x\n")
			}
			r := &fakeRunner{out: map[string]string{"bootctl status --no-pager": tt.bootctl}}
			if got := New(root, r).Bootloader(ctx); got != tt.want {
				t.Errorf("Bootloader = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBluetoothAdapters(t *testing.T) {
	list := "Controller 00:11:22:AA:BB:CC desktop [default]\nController 66:77:88:99:AA:BB desktop #2\n"

	root := buildTree(t, "desktop.txt")
	r := &fakeRunner{out: map[string]string{"bluetoothctl list": list}}
	if got := New(root, r).BluetoothAdapters(ctx); !reflect.DeepEqual(got, []string{"00:11:22:aa:bb:cc"}) {
		t.Errorf("from debugfs: %v", got)
	}
	if r.ran("bluetoothctl") {
		t.Error("bluetoothctl ran although debugfs had the address")
	}

	// Without debugfs, bluetoothctl fills in.
	if err := os.RemoveAll(filepath.Join(root, "sys/kernel/debug")); err != nil {
		t.Fatal(err)
	}
	if got := New(root, r).BluetoothAdapters(ctx); !reflect.DeepEqual(got, []string{"00:11:22:aa:bb:cc", "66:77:88:99:aa:bb"}) {
		t.Errorf("from bluetoothctl: %v", got)
	}

	// Without an adapter, bluetoothctl isn't asked (it can hang without bluetoothd).
	if err := os.RemoveAll(filepath.Join(root, "sys/class/bluetooth")); err != nil {
		t.Fatal(err)
	}
	r = &fakeRunner{out: map[string]string{"bluetoothctl list": list}}
	if got := New(root, r).BluetoothAdapters(ctx); len(got) != 0 || r.ran("bluetoothctl") {
		t.Errorf("without an adapter: %v, ran %v", got, r.calls)
	}
}

func TestModulesWithoutModuleSupport(t *testing.T) {
	root := buildTree(t, "desktop.txt")
	if err := os.Remove(filepath.Join(root, "proc/modules")); err != nil {
		t.Fatal(err)
	}
	mods, err := New(root, desktopRunner()).Modules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"btusb", "example_gpu", "xhci_pci"}; !reflect.DeepEqual(mods, want) {
		t.Errorf("Modules = %v, want %v", mods, want)
	}
}

func TestModulesIgnoresFailures(t *testing.T) {
	root := buildTree(t, "desktop.txt")
	r := desktopRunner()
	r.fail = map[string]bool{"modprobe -R pci:v00001234d00001111sv00001234sd00000001bc0Csc03i30": true}
	r.out["modprobe -R pci:v00001234d00001111sv00001234sd00000001bc0Csc03i30"] = ""
	mods, err := New(root, r).Modules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"btmtk", "btusb", "example_gpu", "snd_hda_intel"}
	if !reflect.DeepEqual(mods, want) {
		t.Errorf("Modules = %v, want %v", mods, want)
	}
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, "modprobe") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("modprobe ran %d times, want once per modalias (3): %v", n, r.calls)
	}
}

func TestDevicesForModule(t *testing.T) {
	s := New(buildTree(t, "desktop.txt"), nil)
	for mod, want := range map[string][]string{
		"btusb":       {"usb:0a0b:0c0d"}, // bound to an interface: the device it belongs to
		"example_gpu": {"pci:1234:abcd"},
		"btmtk":       {}, // a helper: drives nothing itself
		"missing":     {},
	} {
		if got := s.DevicesForModule(mod); !reflect.DeepEqual(got, want) {
			t.Errorf("DevicesForModule(%q) = %v, want %v", mod, got, want)
		}
	}
	if got := s.ModuleHolders("btmtk"); !reflect.DeepEqual(got, []string{"btusb"}) {
		t.Errorf("ModuleHolders(btmtk) = %v", got)
	}
}

func TestDeviceName(t *testing.T) {
	r := &fakeRunner{out: map[string]string{
		"lspci -mm -d 1234:abcd": `00:02.0 "VGA compatible controller [0300]" "Example Corp [1234]" "Example Graphics 500 [abcd]" -r01 "Example Corp" "Device [0001]"` + "\n",
		"lspci -mm -d 1234:9999": "",
		"lsusb -d 0a0b:0c0d":     "Bus 001 Device 003: ID 0a0b:0c0d Example Corp. Wireless Adapter\n",
	}}
	s := New(t.TempDir(), r)
	for id, want := range map[string]string{
		"pci:1234:abcd":  "Example Graphics 500",
		"pci:1234:9999":  "pci:1234:9999",
		"usb:0a0b:0c0d":  "Example Corp. Wireless Adapter",
		"usb:0a0b:9999":  "usb:0a0b:9999",
		"module:btusb":   "module:btusb",
		"bt:00:11:22:33": "bt:00:11:22:33",
	} {
		if got := s.DeviceName(ctx, id); got != want {
			t.Errorf("DeviceName(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestFingerprintReaders(t *testing.T) {
	root := buildTree(t, "desktop.txt")
	// 0f0f:0101 is named like a reader in sysfs, 0e0e:0202 is listed in the hwdb, lsusb's name
	// gives 0c0c:1b2c away, and 0a0b:0c0d is none.
	r := &fakeRunner{out: map[string]string{"lsusb": "Bus 001 Device 006: ID 0c0c:1b2c Example Fingerprint Reader\n" +
		"Bus 001 Device 003: ID 0a0b:0c0d Example Corp. Wireless Adapter\n" +
		"Bus 002 Device 002: ID 0d0d:0303 Example FPrint (not plugged in)\n"}}
	want := []string{"usb:0c0c:1b2c", "usb:0e0e:0202", "usb:0f0f:0101"}
	if got := New(root, r).FingerprintReaders(ctx); !reflect.DeepEqual(got, want) {
		t.Errorf("FingerprintReaders = %v, want %v", got, want)
	}
}

func TestAppDevices(t *testing.T) {
	present := []string{"usb:0c0c:1b2c", "usb:0c0c:2000", "usb:0a0b:0c0d", "pci:0c0c:1b2c"}
	tests := []struct {
		name  string
		rules []string
		want  []string
	}{
		{"vendor and product", []string{`SUBSYSTEM=="usb", ATTRS{idVendor}=="0C0C", ATTRS{idProduct}=="1b2c", MODE="0666"`}, []string{"usb:0c0c:1b2c"}},
		{"whole vendor", []string{`SUBSYSTEMS=="usb", ATTRS{idVendor}=="0c0c", TAG+="uaccess"`}, []string{"usb:0c0c:1b2c", "usb:0c0c:2000"}},
		{"several files", []string{
			`ATTRS{idVendor}=="0a0b", ATTRS{idProduct}=="0c0d", MODE="0666"`,
			`ATTR{idVendor}=="0c0c", ATTR{idProduct}=="2000", MODE="0666"`,
		}, []string{"usb:0a0b:0c0d", "usb:0c0c:2000"}},
		{"too many makers", []string{
			`ATTRS{idVendor}=="0c0c", ATTRS{idProduct}=="1b2c"`,
			`ATTRS{idVendor}=="1111"` + "\n" + `ATTRS{idVendor}=="2222"` + "\n" + `ATTRS{idVendor}=="3333"`,
		}, nil},
		{"three makers is fine", []string{
			`ATTRS{idVendor}=="0c0c", ATTRS{idProduct}=="1b2c"` + "\n" + `ATTRS{idVendor}=="1111"` + "\n" + `ATTRS{idVendor}=="2222"`,
		}, []string{"usb:0c0c:1b2c"}},
		{"no usb rules", []string{`KERNEL=="uinput", MODE="0660"`}, nil},
		{"nothing", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AppDevices(tt.rules, present)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AppDevices = %v, want %v", got, tt.want)
			}
		})
	}
}
