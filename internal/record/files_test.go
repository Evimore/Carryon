package record

import "testing"

func TestReviewOnly(t *testing.T) {
	for path, want := range map[string]bool{
		"/etc/fstab":              true,
		"/etc/crypttab":           true,
		"/etc/default/grub":       true,
		"/etc/default/limine":     true,
		"/etc/kernel/cmdline":     true,
		"/etc/sdboot-manage.conf": true,
		"/etc/fstab.bak":          false,
		"/etc/fstab.d/extra":      false,
		"/etc/default":            false,
		"/etc/mkinitcpio.conf":    false, // copied after the boot-risk check, not review-only
		"/boot/limine.conf":       false, // not captured: /boot isn't part of the record
		"fstab":                   false,
	} {
		if got := ReviewOnly(path); got != want {
			t.Errorf("ReviewOnly(%q) = %v, want %v", path, got, want)
		}
	}
}
