package pacman

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Evimore/carryon/internal/pkgmgr"
)

func TestDetectDistro(t *testing.T) {
	tests := []struct {
		name, file, text string
		want             Distro
	}{
		{"arch", "etc/os-release", "NAME=\"Arch Linux\"\nID=arch\n", Distro{ID: "arch"}},
		{"cachyos", "etc/os-release", "NAME=\"CachyOS Linux\"\nID=cachyos\nID_LIKE=arch\n",
			Distro{ID: "cachyos", IDLike: []string{"arch"}}},
		{"endeavouros", "etc/os-release", "NAME='EndeavourOS'\nID='endeavouros'\nID_LIKE='arch'\n",
			Distro{ID: "endeavouros", IDLike: []string{"arch"}}},
		{"manjaro", "usr/lib/os-release", "NAME=\"Manjaro Linux\"\nID=manjaro\nID_LIKE=arch\n",
			Distro{ID: "manjaro", IDLike: []string{"arch"}}},
		{"garuda", "etc/os-release", "# comment\nNAME=\"Garuda Linux\"\nID=garuda\nID_LIKE=\"arch\"\n",
			Distro{ID: "garuda", IDLike: []string{"arch"}}},
		{"several likes", "etc/os-release", "ID=example\nID_LIKE=\"cachyos arch\"\n",
			Distro{ID: "example", IDLike: []string{"cachyos", "arch"}}},
		{"none", "", "", Distro{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.file != "" {
				full := filepath.Join(root, tt.file)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(tt.text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := DetectDistro(root)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStandardRepos(t *testing.T) {
	arch := Distro{ID: "arch"}
	like := func(id string) Distro { return Distro{ID: id, IDLike: []string{"arch"}} }
	tests := []struct {
		distro Distro
		extra  []string
		repo   string
		want   bool
	}{
		// Arch's repositories are standard everywhere, like the prototype's list
		{arch, nil, "core", true},
		{arch, nil, "extra", true},
		{arch, nil, "multilib", true},
		{arch, nil, "core-testing", true},
		{arch, nil, "extra-testing", true},
		{arch, nil, "multilib-testing", true},
		{arch, nil, "extra-staging", true},
		{arch, nil, "gnome-unstable", true},
		{arch, nil, "kde-unstable", true},
		{arch, nil, "community", true},
		{arch, nil, "testing", true},
		{like("cachyos"), nil, "extra", true},
		{like("garuda"), nil, "multilib", true},
		// a distribution's own repositories are standard only there
		{arch, nil, "cachyos", false},
		{arch, nil, "chaotic-aur", false},
		{arch, nil, "endeavouros", false},
		{like("cachyos"), nil, "cachyos", true},
		{like("cachyos"), nil, "cachyos-v3", true},
		{like("cachyos"), nil, "cachyos-core-v3", true},
		{like("cachyos"), nil, "cachyos-extra-znver4", true},
		{like("cachyos"), nil, "endeavouros", false},
		{like("endeavouros"), nil, "endeavouros", true},
		{like("endeavouros"), nil, "endeavouros-testing-dev", true},
		{like("endeavouros"), nil, "chaotic-aur", false},
		{like("manjaro"), nil, "core", true},
		{like("manjaro"), nil, "community", true},
		{like("garuda"), nil, "garuda", true},
		{like("garuda"), nil, "chaotic-aur", true},
		// a distribution based on another gets that one's repositories too
		{Distro{ID: "example", IDLike: []string{"cachyos", "arch"}}, nil, "cachyos-v4", true},
		// never standard unless the config says so
		{arch, nil, "example", false},
		{arch, nil, "core-example", false},
		{arch, []string{"example"}, "example", true},
		{Distro{}, nil, "core", true},
		{Distro{}, nil, "example", false},
	}
	for _, tt := range tests {
		if got := StandardRepos(tt.distro, tt.extra)(tt.repo); got != tt.want {
			t.Errorf("StandardRepos(%+v, %v)(%q) = %v, want %v", tt.distro, tt.extra, tt.repo, got, tt.want)
		}
	}
}

func TestExtraRepos(t *testing.T) {
	root, err := filepath.Abs("testdata/root")
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	f.file("pacman-conf --repo-list", "repo-list.txt", nil)

	got, err := New(f, Options{Root: root}).ExtraRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []pkgmgr.Repo{
		// pacman.conf comes before pacman.d/example.conf, so its [example] wins
		{Name: "example", Section: "[example]\nSigLevel = Required\nServer = https://repo.example.com/$repo/$arch\n"},
		// testdata/root is Arch, so CachyOS's repository added by hand is extra
		{Name: "cachyos", Conf: "/etc/pacman.d/cachyos.conf", Section: "[cachyos]\nInclude = /etc/pacman.d/cachyos-mirrorlist\n"},
		{Name: "missing-section"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtraRepos:\n got %+v\nwant %+v", got, want)
	}

	got, err = New(f, Options{Root: root, ExtraStandardRepos: []string{"example", "cachyos", "missing-section"}}).ExtraRepos(ctx)
	if err != nil || got != nil {
		t.Errorf("with every repository standard: %+v, %v", got, err)
	}
}

func TestConfiguredReposError(t *testing.T) {
	f := newFake(t)
	f.replies["pacman-conf --repo-list"] = reply{nil, exitError(1)}
	if _, err := New(f, Options{}).ExtraRepos(ctx); err == nil {
		t.Error("want an error when pacman-conf fails")
	}
}

func TestRepoSectionsComments(t *testing.T) {
	// a section runs to the next header, comments and blank lines included, then gets trimmed
	root := t.TempDir()
	conf := "[options]\nArchitecture = auto\n\n  [example]   # mine\nServer = https://repo.example.com/$arch\n# CacheServer = https://cache.example.com\n\n[core]\nInclude = /etc/pacman.d/mirrorlist\n"
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/pacman.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err := New(nil, Options{Root: root}).repoSections([]string{"example"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]repoDef{"example": {"/etc/pacman.conf",
		"[example]   # mine\nServer = https://repo.example.com/$arch\n# CacheServer = https://cache.example.com\n"}}
	if !reflect.DeepEqual(defs, want) {
		t.Errorf("got %q, want %q", defs, want)
	}
}
