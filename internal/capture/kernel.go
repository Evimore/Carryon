package capture

import (
	"regexp"
	"strings"

	"github.com/Evimore/carryon/internal/record"
)

// CmdlineSource is a config file the persistent kernel command line can live in.
type CmdlineSource struct {
	Path    string         // absolute
	Pattern *regexp.Regexp // group 1: what comes before the parameters; group 2: the parameters
	Apply   []string       // the command that turns the file into the boot config
}

// CmdlineSources are tried in order; the first file that exists holds the parameters. From the
// prototype's CMDLINE_SOURCES.
var CmdlineSources = []CmdlineSource{
	{"/etc/default/limine", regexp.MustCompile(`(?m)^(\s*KERNEL_CMDLINE\[default\]\+?=["'])([^"']*)`), []string{"limine-update"}},
	{"/etc/sdboot-manage.conf", regexp.MustCompile(`(?m)^(\s*LINUX_OPTIONS=["'])([^"']*)`), []string{"sdboot-manage", "gen"}},
	{"/etc/kernel/cmdline", regexp.MustCompile(`(?m)^()(.*)$`), []string{"mkinitcpio", "-P"}},
	{"/etc/default/grub", regexp.MustCompile(`(?m)^(\s*GRUB_CMDLINE_LINUX(?:_DEFAULT)?=["'])([^"']*)`), []string{"grub-mkconfig", "-o", "/boot/grub/grub.cfg"}},
}

// rootish matches parameters that differ per install (disks, root, resume), never recorded.
var rootish = regexp.MustCompile(`^(BOOT_IMAGE|initrd|root|rootflags|rootfstype|cryptdevice|cryptkey|resume|` +
	`resume_offset|rd\.luks\.[a-z.]+|rw|ro)(=|$)`)

// procCmdline is read when no bootloader config is found.
const procCmdline = "/proc/cmdline"

// cmdlineConfig returns where the persistent kernel command line lives, and its parameters. When no
// config file is found, it reads the running kernel's command line. Ported from cmdline_config.
func (c *capturer) cmdlineConfig() (string, []string) {
	for _, s := range CmdlineSources {
		b, err := c.readFile(c.path(s.Path))
		if err != nil {
			continue
		}
		var params []string
		for _, m := range s.Pattern.FindAllStringSubmatch(string(b), -1) {
			params = append(params, strings.Fields(m[2])...)
		}
		return s.Path, params
	}
	b, _ := c.readFile(c.path(procCmdline))
	return procCmdline, strings.Fields(string(b))
}

// captureKernel records the kernel parameters, minus the ones that differ per install. Ported
// from capture_kernel.
func (c *capturer) captureKernel() {
	src, params := c.cmdlineConfig()
	seen := map[string]bool{}
	for _, p := range params {
		if seen[p] || rootish.MatchString(p) {
			continue
		}
		seen[p] = true
		c.add(record.Item{Type: record.TypeKparam, ID: p, Source: src})
	}
}
