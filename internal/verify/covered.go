package verify

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/record"
)

// checkCoverage checks that what the machine runs is covered by the backup: the programs units
// start, DKMS driver sources, kernel parameters, mounts and kernel modules. It also lists leftover
// enable links and files the capture couldn't read.
func (v *verifier) checkCoverage(ctx context.Context) {
	v.rep.section("Is everything custom backed up?")
	v.checkUnitPrograms(ctx)
	v.checkDKMS(ctx)
	v.checkKernelParams()
	v.checkMounts()
	v.checkModules(ctx)
	v.checkEnableLinks()
	if data, err := os.ReadFile(filepath.Join(v.opt.Dirs.RecordDir(), capture.SkippedFile)); err == nil {
		if skipped := strings.Fields(string(data)); len(skipped) > 0 {
			v.rep.add(Line{Level: Warning, Text: "Some files couldn't be read when the record was captured:", Details: skipped,
				Fix: "Capture as root to include them: sudo carryon backup"})
		}
	}
}

var (
	// execPath finds the programs in `systemctl show -p ExecStart...` output.
	execPath = regexp.MustCompile(`path=(\S+)`)
	// execLine finds the program of an Exec line in a unit file.
	execLine = regexp.MustCompile(`(?m)^\s*Exec[A-Za-z]*=[-@:+!|]*(\S+)`)
)

// checkUnitPrograms checks that the programs hand-made units start come from a package or are in
// the backup.
func (v *verifier) checkUnitPrograms(ctx context.Context) {
	runs := map[string][]string{} // unit -> the programs it starts
	var units []string
	for _, it := range v.rec.Items {
		if it.Type == record.TypeSvc && it.Pkg == "" {
			units = append(units, it.ID)
		}
	}
	if len(units) > 0 {
		args := append([]string{"show", "-p", "ExecStartPre", "-p", "ExecStart", "-p", "ExecStartPost", "--"}, units...)
		out, err := v.opt.System.Runner.Output(ctx, "systemctl", args...)
		if err != nil && len(out) == 0 {
			v.rep.add(Line{Level: Warning, Text: "The services' programs couldn't be read: " + err.Error()})
		}
		// one block per unit, in the order asked, separated by blank lines
		for i, b := range strings.Split(strings.TrimSpace(string(out)), "\n\n") {
			if i < len(units) {
				for _, m := range execPath.FindAllStringSubmatch(b, -1) {
					runs[units[i]] = append(runs[units[i]], m[1])
				}
			}
		}
	}
	for _, u := range v.userUnitFiles() {
		data, err := os.ReadFile(u.file)
		if err != nil {
			continue
		}
		name := "user:" + u.user + ":" + filepath.Base(u.file)
		for _, m := range execLine.FindAllStringSubmatch(string(data), -1) {
			runs[name] = append(runs[name], strings.ReplaceAll(m[1], "%h", u.home))
		}
	}

	var all []string
	for _, ps := range runs {
		for _, p := range ps {
			if strings.HasPrefix(p, "/") {
				all = append(all, p)
			}
		}
	}
	owners, err := v.opt.System.Packages.Owners(ctx, all)
	if err != nil {
		v.rep.add(Line{Level: Warning, Text: "The packages that own the units' programs couldn't be read: " + err.Error()})
		return
	}
	names := make([]string, 0, len(runs))
	for u := range runs {
		names = append(names, u)
	}
	sort.Strings(names)
	var bad []string
	for _, u := range names {
		for _, p := range runs[u] {
			if strings.HasPrefix(p, "/") && owners[p] == "" && !v.covered(p) {
				bad = append(bad, u+" runs "+p)
			}
		}
	}
	v.rep.check(Problem, bad,
		fmt.Sprintf("The programs your %d hand-made units start are all backed up.", len(names)),
		"Some programs your hand-made units start aren't in the backup:",
		"Move them somewhere backed up (such as /usr/local/bin), or add their folder to backup.extra_paths.")
}

// unitFile is a user unit file saved in the record.
type unitFile struct{ user, home, file string }

// userUnitFiles lists the user unit files the record holds, with whose they are.
func (v *verifier) userUnitFiles() []unitFile {
	base := filepath.Join(v.opt.Dirs.RecordDir(), capture.UserUnitsDir)
	var out []unitFile
	for _, u := range v.rec.Meta.AllUsers() {
		_ = filepath.WalkDir(filepath.Join(base, u.Name), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				out = append(out, unitFile{u.Name, u.Home, p})
			}
			return nil
		})
	}
	return out
}

// covered reports whether a file is in the backup: copied into the record, or inside a backed-up
// folder and not left out by the exclude list. Ported from covered.
func (v *verifier) covered(p string) bool {
	if _, err := os.Lstat(filepath.Join(v.opt.Dirs.RecordDir(), capture.FilesDir, p)); err == nil {
		return true
	}
	for _, t := range v.backedUp() {
		if (p == t || strings.HasPrefix(p, strings.TrimSuffix(t, "/")+"/")) && !v.isExcluded(p) {
			return true
		}
	}
	return false
}

// backedUp lists the folders every backup holds: the system folders, the record's backed-up paths,
// backup.extra_paths and the users' homes.
func (v *verifier) backedUp() []string {
	out := append([]string(nil), backup.SystemPaths...)
	for _, it := range v.rec.Items {
		if it.Type == record.TypePath && filepath.IsAbs(it.ID) && !strings.ContainsAny(it.ID, "*?[") {
			out = append(out, it.ID)
		}
	}
	out = append(out, v.opt.Config.Backup.ExtraPaths...)
	for _, u := range v.rec.Meta.AllUsers() {
		if u.Home != "" {
			out = append(out, u.Home)
		}
	}
	return out
}

// dkmsLine matches a module in `dkms status`: "name/version, kernel, arch: installed" (or
// "name/version: added").
var dkmsLine = regexp.MustCompile(`(?m)^([^/,\s]+)/([^,:\s]+)`)

// checkDKMS checks that DKMS driver sources come from packages or are in the record.
func (v *verifier) checkDKMS(ctx context.Context) {
	run := v.opt.System.Runner
	if !run.Exists("dkms") {
		return
	}
	out, err := run.Output(ctx, "dkms", "status")
	if err != nil && len(out) == 0 {
		v.rep.add(Line{Level: Warning, Text: "The DKMS drivers couldn't be listed: " + err.Error()})
		return
	}
	seen := map[string]bool{}
	var srcs []string
	for _, m := range dkmsLine.FindAllStringSubmatch(string(out), -1) {
		s := "/usr/src/" + m[1] + "-" + m[2]
		if !seen[s] {
			seen[s] = true
			srcs = append(srcs, s)
		}
	}
	if len(srcs) == 0 {
		return
	}
	sort.Strings(srcs)
	owners, err := v.opt.System.Packages.Owners(ctx, srcs)
	if err != nil {
		v.rep.add(Line{Level: Warning, Text: "The packages that own the DKMS sources couldn't be read: " + err.Error()})
		return
	}
	var bad, from []string
	for _, s := range srcs {
		if owners[s] != "" {
			from = append(from, owners[s])
			continue
		}
		if _, err := os.Stat(filepath.Join(v.opt.Dirs.RecordDir(), capture.FilesDir, s)); err != nil {
			bad = append(bad, s+": no package provides it")
		}
	}
	v.rep.check(Problem, bad,
		"The DKMS driver sources come from packages ("+record.Brief(from, 3)+").",
		"Some DKMS driver sources aren't from a package or in the backup:",
		"Install the driver from a package (often name-dkms), so a new machine can build it.")
}

// checkKernelParams checks that the running kernel's parameters are all in a saved config file.
func (v *verifier) checkKernelParams() {
	data, err := os.ReadFile(filepath.Join(v.root(), "proc/cmdline"))
	if err != nil {
		return
	}
	saved := map[string]bool{}
	for _, it := range v.rec.Items {
		if it.Type == record.TypeKparam {
			saved[it.ID] = true
		}
	}
	var bad []string
	for _, p := range strings.Fields(string(data)) {
		if !capture.PerInstallParam(p) && !saved[p] {
			bad = append(bad, p+": active now but not saved in a config file")
		}
	}
	v.rep.check(Problem, bad,
		"The running kernel's parameters are all in a saved config file.",
		"Some running kernel parameters aren't saved anywhere:",
		"Add them to your bootloader's config (such as /etc/default/limine or /etc/kernel/cmdline), so the next backup records them.")
}

// pseudoFS are filesystems the kernel or a service makes by itself.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true, "securityfs": true,
	"cgroup": true, "cgroup2": true, "pstore": true, "efivarfs": true, "bpf": true, "debugfs": true,
	"tracefs": true, "configfs": true, "fusectl": true, "mqueue": true, "hugetlbfs": true, "autofs": true,
	"binfmt_misc": true, "ramfs": true, "nsfs": true, "fuse.portal": true, "fuse.gvfsd-fuse": true,
	"squashfs": true, "overlay": true, "rpc_pipefs": true, "nfsd": true, "selinuxfs": true,
}

// checkMounts checks that every real mount is recreated by fstab or a saved unit.
func (v *verifier) checkMounts() {
	mounts := v.opt.System.Mounts
	if mounts == nil {
		var err error
		if mounts, err = dest.ReadMounts(v.root()); err != nil {
			v.rep.add(Line{Level: Warning, Text: "The mounts couldn't be checked: " + err.Error()})
			return
		}
	}
	recDir := v.opt.Dirs.RecordDir()
	fstab := map[string]bool{}
	if data, err := os.ReadFile(filepath.Join(recDir, capture.FilesDir, "etc/fstab")); err == nil {
		for _, ln := range strings.Split(string(data), "\n") {
			f := strings.Fields(ln)
			if len(f) > 1 && !strings.HasPrefix(f[0], "#") {
				fstab[fstabUnescape.Replace(f[1])] = true
			}
		}
	}
	var units strings.Builder
	readAll := func(dir string) {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if b, err := os.ReadFile(p); err == nil {
					units.WriteString(d.Name() + "\n") // a mount unit's name is its mount point
					units.Write(b)
					units.WriteByte('\n')
				}
			}
			return nil
		})
	}
	readAll(filepath.Join(recDir, capture.UserUnitsDir))
	readAll(filepath.Join(recDir, capture.FilesDir, "etc/systemd"))
	text := units.String()

	seen := map[string]bool{}
	var bad []string
	for _, m := range mounts {
		t := m.MountPoint
		if seen[t] || pseudoFS[m.FSType] || fstab[t] || underAny(t, "/proc", "/sys", "/dev", "/run") {
			continue
		}
		seen[t] = true
		if strings.Contains(text, t) || mountUnitName(t) != "" && strings.Contains(text, mountUnitName(t)) {
			continue
		}
		if rel, ok := v.homeRel(t); ok && strings.Contains(text, "%h/"+rel) {
			continue
		}
		bad = append(bad, fmt.Sprintf("%s (%s): nothing saved mounts this again", t, m.FSType))
	}
	v.rep.check(Problem, bad,
		"Every mount is recreated by fstab or a saved unit.",
		"Some mounts wouldn't come back on a new machine:",
		"Add them to /etc/fstab or a mount unit, so the next backup records how to mount them.")
}

// fstabUnescape undoes fstab's octal escapes for spaces, tabs and backslashes.
var fstabUnescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// mountUnitName is systemd's unit name for a mount point, roughly (systemd-escape --path):
// /mnt/data is mnt-data.mount. Mount points needing escapes give "".
func mountUnitName(p string) string {
	p = strings.Trim(p, "/")
	if p == "" || strings.ContainsAny(p, "-\\ ") {
		return ""
	}
	return strings.ReplaceAll(p, "/", "-") + ".mount"
}

// homeRel is a path relative to the home of the user it's in.
func (v *verifier) homeRel(p string) (string, bool) {
	for _, u := range v.rec.Meta.AllUsers() {
		if u.Home != "" && strings.HasPrefix(p, u.Home+"/") {
			return strings.TrimPrefix(p, u.Home+"/"), true
		}
	}
	return "", false
}

func underAny(p string, dirs ...string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// checkModules checks that the loaded kernel modules come from packages or DKMS. Others were
// built or copied by hand, and a new machine won't have them.
func (v *verifier) checkModules(ctx context.Context) {
	data, err := os.ReadFile(filepath.Join(v.root(), "proc/modules"))
	if err != nil {
		return
	}
	run := v.opt.System.Runner
	if !run.Exists("modinfo") {
		return
	}
	files := map[string]string{}
	var mods, abs []string
	for _, ln := range strings.Split(string(data), "\n") {
		f := strings.Fields(ln)
		if len(f) == 0 {
			continue
		}
		out, _ := run.Output(ctx, "modinfo", "-n", f[0])
		p := strings.TrimSpace(string(out))
		if strings.HasPrefix(p, "/") {
			mods = append(mods, f[0])
			files[f[0]] = p
			abs = append(abs, p)
		}
	}
	owners, err := v.opt.System.Packages.Owners(ctx, abs)
	if err != nil {
		v.rep.add(Line{Level: Warning, Text: "The packages that own the kernel modules couldn't be read: " + err.Error()})
		return
	}
	var stray []string
	for _, m := range mods {
		p := files[m]
		if owners[p] == "" && !strings.Contains(p, "/updates/dkms/") {
			stray = append(stray, m+": "+p)
		}
	}
	v.rep.check(Warning, stray,
		"The loaded kernel modules all come from packages or DKMS.",
		"Some loaded kernel modules don't come from a package or DKMS:",
		"A new machine won't have these: install them from a package or as a DKMS module.")
}

// checkEnableLinks lists enable links to units that are gone, usually left by a removed package.
func (v *verifier) checkEnableLinks() {
	root := v.root()
	globs := []string{"/etc/systemd/system/*.wants/*"}
	for _, u := range v.rec.Meta.AllUsers() {
		if u.Home != "" {
			globs = append(globs, path.Join(u.Home, capture.UserUnitDir, "*.wants/*"))
		}
	}
	var dead []string
	for _, g := range globs {
		links, _ := filepath.Glob(filepath.Join(root, g))
		for _, l := range links {
			fi, err := os.Lstat(l)
			if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
				continue
			}
			target, err := os.Readlink(l)
			if err != nil {
				continue
			}
			resolved := target
			if filepath.IsAbs(target) {
				resolved = filepath.Join(root, target)
			} else {
				resolved = filepath.Join(filepath.Dir(l), target)
			}
			if _, err := os.Stat(resolved); err != nil {
				rel, _ := filepath.Rel(root, l)
				dead = append(dead, "/"+filepath.ToSlash(rel)+" -> "+target)
			}
		}
	}
	v.rep.check(Warning, dead,
		"No leftover enable links to removed units.",
		"Some enable links point to units that are gone (it's safe to delete them):",
		"")
}
