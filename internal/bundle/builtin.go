package bundle

import (
	"regexp"
	"strings"

	"github.com/Evimore/carryon/internal/record"
)

// Info is a built-in bundle's title and the when-rule it starts with.
type Info struct {
	Title, When string
}

// Builtin are the bundles Carryon always knows, by ID. Titles and when-rules are the prototype's,
// except "packages" and "aur", whose titles named one distribution and one AUR helper.
var Builtin = map[string]Info{
	"packages":      {"Packages from your distribution's repositories", "always"},
	"aur":           {"AUR packages (rebuilt with paru or yay)", "always"},
	"repos":         {"Extra package repositories", "always"},
	"services":      {"Enabled system services", "always"},
	"user-services": {"Enabled user services", "always"},
	"configs":       {"Tweaked package settings", "always"},
	"other-files":   {"Other hand-installed system files", "always"},
	"boot":          {"Initramfs & kernel module settings", "samehost"},
	"kparams":       {"Kernel parameters", "always"},
	"locale":        {"Locale, keyboard & timezone", "always"},
	"account":       {"Group memberships & login shell", "always"},
	"login":         {"Login screen & automatic login", "samehost"},
	"fingerprint":   {"Fingerprint login: reader driver, fingerprint for sudo/polkit, enrolled prints", "samehost"},
	"xkb":           {"Keyboard tweaks (/etc/xkb)", "always"},
	"boot-limine":   {"Limine bootloader tools & snapshot boot entries", "bootloader:limine"},
	"boot-grub":     {"GRUB bootloader tools", "bootloader:grub"},
	"snapshots":     {"Btrfs snapshots (snapper)", "rootfs:btrfs"},
	"dev-tools":     {"Language tools (cargo, pipx, npm, flatpak)", "always"},
	"network":       {"Wi-Fi & VPN connections (with passwords)", "always"},
	"bluetooth":     {"Bluetooth pairings", "samehost"},
	"identity":      {"Machine identity: hostname, SSH host keys, Tailscale node", "samehost"},
	"root-home":     {"root's home directory", "samehost"},
	"srv":           {"/srv", "samehost"},
	"other-paths":   {"Other backed-up system paths", "samehost"},
	"home-keys":     {"Home: SSH & GPG keys", "always"},
	"home-saves":    {"Home: Steam data & Proton game saves", "always"},
	"home-files":    {"Home: your files (Documents, Pictures, Projects...)", "always"},
	"home-apps":     {"Home: app settings & data (dotfiles)", "always"},
	"home-display":  {"Home: monitor layout", "samehost"},
	"disk-layout":   {"Disk layout & bootloader config (shown as a diff, never copied)", "samehost"},
}

// Order is the restore order. "*apps" stands for every app bundle, "*hw" for every hardware
// bundle (kparam-..., firmware-...).
var Order = []string{
	"repos", "packages", "aur", "*apps", "services", "user-services", "configs", "other-files", "*hw",
	"boot-limine", "boot-grub", "snapshots", "boot", "kparams", "locale", "xkb", "account", "login", "fingerprint",
	"dev-tools", "network", "bluetooth", "identity", "root-home",
	"srv", "other-paths", "home-keys", "home-saves", "home-files", "home-apps", "home-display", "disk-layout",
}

// Rank is a bundle's place in the restore order: built-in bundles by Order, hardware bundles
// where "*hw" is and every other bundle where "*apps" is. Bundles of the same rank go by ID.
func Rank(id string) int {
	for i, b := range Order {
		if b == id {
			return i
		}
	}
	if strings.HasPrefix(id, "kparam-") || strings.HasPrefix(id, "firmware-") {
		return rankOf("*hw")
	}
	return rankOf("*apps")
}

func rankOf(id string) int {
	for i, b := range Order {
		if b == id {
			return i
		}
	}
	return len(Order)
}

// wellKnown is a rule putting items of one type whose ID matches into a bundle. With home set, it
// matches the path inside the user's home instead ("~/" + Home, without "~/"), so it holds for
// every user. The prototype matched its one user's absolute paths.
type wellKnown struct {
	bundle, typ string
	rx          *regexp.Regexp
	home        bool
}

func (r wellKnown) matches(it record.Item) bool {
	if it.Type != r.typ {
		return false
	}
	if r.home {
		return it.Home != "" && r.rx.MatchString(it.Home)
	}
	return r.rx.MatchString(it.ID)
}

// wellKnownRules are the prototype's early_rules: well-known places, first match wins.
var wellKnownRules = []wellKnown{
	{"locale", "file", regexp.MustCompile(`^/etc/(locale\.(conf|gen)|vconsole\.conf|timezone|localtime|default/(keyboard|locale)` +
		`|X11/xorg\.conf\.d/00-keyboard\.conf)$`), false},
	{"identity", "file", regexp.MustCompile(`^/etc/(hostname|hosts|machine-info)$`), false},
	{"identity", "path", regexp.MustCompile(`^/(etc/ssh/ssh_host_\*|var/lib/tailscale)$`), false},
	{"root-home", "path", regexp.MustCompile(`^/root$`), false},
	{"network", "path", regexp.MustCompile(`^/etc/NetworkManager/system-connections$`), false},
	{"bluetooth", "path", regexp.MustCompile(`^/var/lib/bluetooth$`), false},
	{"srv", "path", regexp.MustCompile(`^/srv$`), false},
	{"home-keys", "path", regexp.MustCompile(`^\.(ssh|gnupg|pki|password-store)$`), true},
	{"home-saves", "path", regexp.MustCompile(`^\.local/share/Steam$`), true},
	{"home-display", "path", regexp.MustCompile(`^(\.config/(kwinoutputconfig\.json|monitors\.xml)|\.local/share/kscreen)$`), true},
	{"home-files", "path", regexp.MustCompile(`^[^./][^/]*$`), true},
	{"fingerprint", "path", regexp.MustCompile(`^/var/lib/fprint$`), false},
	{"fingerprint", "pkg", regexp.MustCompile(`^(fprintd|libfprint.*)$`), false},
	{"fingerprint", "aur", regexp.MustCompile(`(fprint|fingerprint)`), false},
	{"xkb", "file", regexp.MustCompile(`^/etc/xkb/`), false},
	// autologin settings would let anyone in on another machine
	{"login", "file", regexp.MustCompile(`^/etc/(plasmalogin\.conf|sddm\.conf(\.d/.*)?|lightdm/lightdm\.conf|gdm/custom\.conf|ly/config\.ini|greetd/config\.toml)$`), false},
	{"login", "group", regexp.MustCompile(`^(nopasswdlogin|autologin)$`), false},
	// bootloader and filesystem specific: only where that bootloader or btrfs is in use
	{"boot-limine", "pkg", regexp.MustCompile(`^limine(-.*)?$`), false},
	{"boot-limine", "file", regexp.MustCompile(`^/etc/(limine-snapper-sync\.conf|limine-entry-tool\.conf|mkinitcpio\.conf\.d/[^/]*limine[^/]*)$`), false},
	{"boot-limine", "svc", regexp.MustCompile(`^limine-.*\.service$`), false},
	{"boot-grub", "pkg", regexp.MustCompile(`^(grub|grub-.*)$`), false},
	{"snapshots", "pkg", regexp.MustCompile(`^(snapper|snap-pac|btrfs-assistant|cachyos-snapper-support|timeshift(-autosnap)?)$`), false},
	{"snapshots", "file", regexp.MustCompile(`^/etc/(snapper/.*|conf\.d/snapper)$`), false},
	{"snapshots", "svc", regexp.MustCompile(`^(snapper-.*|grub-btrfsd)\.(service|timer)$`), false},
	// initramfs hooks differ between machines (encryption!), so these default to this machine only
	{"boot", "file", regexp.MustCompile(`^/etc/(mkinitcpio\.conf(\.d/.*)?|mkinitcpio\.d/.*|modprobe\.d/.*|modules-load\.d/.*|dracut\.conf(\.d/.*)?)$`), false},
}

// fallback is the catch-all bundle of each item type.
var fallback = map[string]string{
	record.TypePkg: "packages", record.TypeAUR: "aur", record.TypeRepo: "repos",
	record.TypeSvc: "services", record.TypeMask: "services",
	record.TypeUsvc: "user-services", record.TypeUunit: "user-services", record.TypeUmask: "user-services",
	record.TypeLinger: "user-services", record.TypeMkdir: "user-services",
	record.TypeKparam: "kparams", record.TypeGroup: "account", record.TypeShell: "account",
	record.TypeCargo: "dev-tools", record.TypePipx: "dev-tools", record.TypeNpm: "dev-tools", record.TypeFlatpak: "dev-tools",
}

func fallbackOf(typ string) string {
	if b, ok := fallback[typ]; ok {
		return b
	}
	return "other"
}
