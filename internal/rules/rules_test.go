package rules

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/record"
)

func TestGlobFollowsPythonFnmatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"steam", "steam", true},
		{"steam", "steam-native", false},
		{"lib*", "libfprint", true},
		{"~/.local/share/*", "~/.local/share/Steam/userdata", true}, // "*" crosses "/" like fnmatch
		{"*.service", "foo.service", true},
		{"?ar", "bar", true},
		{"?ar", "ar", false},
		{"[abc]x", "bx", true},
		{"[!abc]x", "bx", false},
		{"[!abc]x", "dx", true},
		{"[a-c]x", "cx", true},
		{"[]]", "]", true},
		{"[", "[", true},     // unclosed bracket matches itself
		{"a[b", "a[b", true}, // likewise
		{"[!]", "x", false},  // "[!]" has no closing bracket after the "!]": it is literal
		{"[!]", "[!]", true},
		{"a.b", "axb", false}, // regexp metacharacters are literal
		{"a+b", "a+b", true},
		{"ABC", "abc", false}, // case-sensitive
		{"**/x", "a/b/x", true},
	}
	for _, c := range cases {
		if got := CompileGlob(c.pat).Match(c.name); got != c.want {
			t.Errorf("Glob(%q).Match(%q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

var desktop = Facts{
	Host: "desktop", Machine: "aaaa", Chassis: "desktop", RootFS: "btrfs", Bootloader: "limine",
	IDs: map[string]bool{"pci:1234:abcd": true, "usb:1111:2222": true, "module:btusb": true},
}

func TestEvaluate(t *testing.T) {
	same := record.Meta{Host: "desktop", Machine: "aaaa"}
	renamed := record.Meta{Host: "old-name", Machine: "aaaa"}
	other := record.Meta{Host: "desktop", Machine: "bbbb"}
	cases := []struct {
		rule   string
		src    record.Meta
		ok     bool
		reason string
	}{
		{"always", other, true, ""},
		{"never", same, false, "opt-in"},
		{"samehost", same, true, ""},
		{"samehost", renamed, true, ""}, // same hardware under a new name
		{"samehost", other, false, "specific to desktop (this is a different machine)"},
		{"chassis:desktop", other, true, ""},
		{"chassis:laptop", other, false, "laptops only"},
		{"hw:pci:1234:ABCD", other, true, ""}, // IDs compare in lower case
		{"hw:usb:dead:beef,module:btusb", other, true, ""},
		{"hw:usb:dead:beef,pci:1234:5678,bt:00:11", other, false, "needs USB dead:beef or PCI 1234:5678, not found here"},
		{"rootfs:btrfs", other, true, ""},
		{"rootfs:ext4", other, false, "needs a ext4 root filesystem (this one is btrfs)"},
		{"bootloader:grub", other, false, "needs the grub bootloader (this machine uses limine)"},
		{"rootfs:btrfs+bootloader:limine", other, true, ""},
		{"rootfs:btrfs+bootloader:grub", other, false, "needs the grub bootloader (this machine uses limine)"},
	}
	for _, c := range cases {
		ok, reason, err := Evaluate(c.rule, desktop, c.src)
		if err != nil || ok != c.ok || reason != c.reason {
			t.Errorf("Evaluate(%q) = %v, %q, %v; want %v, %q", c.rule, ok, reason, err, c.ok, c.reason)
		}
	}
}

func TestSamehostFallsBackToHostname(t *testing.T) {
	// Captured without root, a record has no fingerprint: compare hostnames instead.
	f := Facts{Host: "desktop"}
	if ok, _, _ := Evaluate("samehost", f, record.Meta{Host: "desktop"}); !ok {
		t.Error("same hostname without fingerprints should count as the same machine")
	}
	if ok, _, _ := Evaluate("samehost", f, record.Meta{Host: "laptop"}); ok {
		t.Error("a different hostname without fingerprints is a different machine")
	}
}

func TestUnknownRulesCountAsAlways(t *testing.T) {
	for _, rule := range []string{"sometimes", "chassis:", "hw:", "rootfs:btrfs+mood:good"} {
		ok, _, err := Evaluate(rule, desktop, record.Meta{})
		if !ok || !errors.Is(err, ErrUnknownRule) {
			t.Errorf("Evaluate(%q) = %v, %v; want true and ErrUnknownRule", rule, ok, err)
		}
	}
	if err := Check("bootloader:limine"); err != nil {
		t.Errorf("Check(bootloader:limine) = %v", err)
	}
}

func TestParseBundleRules(t *testing.T) {
	conf := `# my rules
title gaming      Gaming stack
title  remote   Remote desktop  (work)
when  gaming      chassis:desktop
when  remote      sometimes
put   gaming      pkg   steam
put   gaming      pkg   lutris*
put   cloud       path  ~/.config/rclone
drop  path        ~/.local/share/huge-cache
drop  oops
`
	r, problems, err := ParseBundleRules(strings.NewReader(conf), "bundles.conf")
	if err != nil {
		t.Fatal(err)
	}
	if r.Titles["gaming"] != "Gaming stack" || r.Titles["remote"] != "Remote desktop  (work)" {
		t.Errorf("titles = %q", r.Titles)
	}
	if r.Whens["gaming"] != "chassis:desktop" {
		t.Errorf("whens = %q", r.Whens)
	}
	if len(r.Puts) != 3 || len(r.Drops) != 1 {
		t.Fatalf("puts = %+v, drops = %+v", r.Puts, r.Drops)
	}
	wantProblems := []string{
		`bundles.conf:5: unknown when-rule "sometimes", treating it as 'always'`,
		"bundles.conf:10: can't parse: drop  oops",
	}
	if !reflect.DeepEqual(problems, wantProblems) {
		t.Errorf("problems = %q, want %q", problems, wantProblems)
	}

	steam := record.Item{Type: record.TypePkg, ID: "steam"}
	lutris := record.Item{Type: record.TypePkg, ID: "lutris-git"}
	rclone := record.Item{Type: record.TypePath, ID: "/home/alice/.config/rclone", Home: ".config/rclone"}
	if !r.Puts[0].Match.Matches(steam) || !r.Puts[1].Match.Matches(lutris) || !r.Puts[2].Match.Matches(rclone) {
		t.Error("put rules should match steam, lutris-git and ~/.config/rclone")
	}
	if r.Puts[0].Match.Matches(record.Item{Type: record.TypeAUR, ID: "steam"}) {
		t.Error("a pkg rule must not match an aur item")
	}
	any := ItemMatch{Type: "*", Glob: CompileGlob("*steam*")}
	if !any.Matches(record.Item{Type: record.TypeAUR, ID: "steam-tui"}) {
		t.Error(`type "*" matches any item type`)
	}
}
