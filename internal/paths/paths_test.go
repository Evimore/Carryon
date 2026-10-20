package paths

import "testing"

func TestDefaultUsesStandardLocations(t *testing.T) {
	for _, v := range []string{"CARRYON_CONF_DIR", "CARRYON_STATE_DIR", "CARRYON_CACHE_DIR", "CARRYON_RUN_DIR", "CARRYON_SHARE_DIR", "CARRYON_MOTD_DIR"} {
		t.Setenv(v, "")
	}
	d := Default()
	want := Dirs{Conf: "/etc/carryon", State: "/var/lib/carryon", Cache: "/var/cache/carryon", Run: "/run/carryon", Share: "/usr/share/carryon", Motd: "/run/motd.d"}
	if d != want {
		t.Fatalf("Default() = %+v, want %+v", d, want)
	}
	if got := d.LoginMessageFile(); got != "/run/motd.d/carryon.motd" {
		t.Errorf("LoginMessageFile() = %q", got)
	}
	if got := (Dirs{}).LoginMessageFile(); got != "" {
		t.Errorf("LoginMessageFile() without a motd folder = %q, want none", got)
	}
	if got := d.RecordDir(); got != "/var/lib/carryon/record" {
		t.Errorf("RecordDir() = %q", got)
	}
}

func TestDefaultHonoursOverrides(t *testing.T) {
	t.Setenv("CARRYON_CONF_DIR", "/tmp/c")
	t.Setenv("CARRYON_STATE_DIR", "/tmp/s")
	d := Default()
	if d.ConfigFile() != "/tmp/c/config.toml" {
		t.Errorf("ConfigFile() = %q", d.ConfigFile())
	}
	if d.RestoringFlag() != "/tmp/s/restoring" {
		t.Errorf("RestoringFlag() = %q", d.RestoringFlag())
	}
}
