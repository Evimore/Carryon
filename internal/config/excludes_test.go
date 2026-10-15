package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAddExclude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "excludes.txt")
	added, err := AddExclude(path, "/usr/share/carryon/excludes.default", " /var/lib/docker ")
	if err != nil || !added {
		t.Fatalf("added = %v, err = %v", added, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "# Paths Carryon leaves out") || !strings.Contains(string(data), "/usr/share/carryon/excludes.default") {
		t.Errorf("no header:\n%s", data)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}

	if added, err := AddExclude(path, "", "/var/lib/docker"); err != nil || added {
		t.Errorf("a duplicate: added = %v, err = %v", added, err)
	}
	if added, err := AddExclude(path, "", "node_modules"); err != nil || !added {
		t.Errorf("added = %v, err = %v", added, err)
	}
	got, err := ReadExcludes(path)
	if err != nil || !slices.Equal(got, []string{"/var/lib/docker", "node_modules"}) {
		t.Errorf("ReadExcludes = %q, %v", got, err)
	}

	for _, bad := range []string{"", "  ", "/a\n/b", "#x"} {
		if _, err := AddExclude(path, "", bad); err == nil {
			t.Errorf("AddExclude(%q) should fail", bad)
		}
	}
}

func TestAddExcludeKeepsTheUsersFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "excludes.txt")
	if err := os.WriteFile(path, []byte("# mine\n/srv/tmp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AddExclude(path, "", "/srv/cache"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "# mine\n/srv/tmp\n/srv/cache\n" {
		t.Errorf("file = %q", data)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestReadExcludesMissingFile(t *testing.T) {
	got, err := ReadExcludes(filepath.Join(t.TempDir(), "none"))
	if err != nil || got != nil {
		t.Errorf("got %q, %v", got, err)
	}
}
