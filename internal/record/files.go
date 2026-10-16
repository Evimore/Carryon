package record

import "slices"

// ReviewOnlyFiles are the system files that name disks by UUID (fstab, crypttab, bootloader
// configs) or hold the kernel command line. They differ on every install, so restore shows them
// as a diff and never copies them, and bundling keeps them in the disk-layout bundle.
var ReviewOnlyFiles = []string{
	"/etc/crypttab", "/etc/default/grub", "/etc/default/limine", "/etc/fstab", "/etc/kernel/cmdline",
	"/etc/sdboot-manage.conf",
}

// ReviewOnly reports whether path is one of ReviewOnlyFiles.
func ReviewOnly(path string) bool { return slices.Contains(ReviewOnlyFiles, path) }
