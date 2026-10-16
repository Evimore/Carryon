package verify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/record"
)

// aurServer is a fake AUR. It knows the packages in pkgs (name -> JSON fields beyond the name).
func aurServer(t *testing.T, pkgs map[string]map[string]any) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc/v5/info" {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		results := []map[string]any{}
		for _, n := range r.URL.Query()["arg[]"] {
			if p, ok := pkgs[n]; ok {
				res := map[string]any{"Name": n, "Maintainer": "alice", "OutOfDate": nil}
				for k, v := range p {
					res[k] = v
				}
				results = append(results, res)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"version": 5, "type": "multiinfo", "resultcount": len(results), "results": results})
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

// repoServer serves the databases of the repositories named, for $arch x86_64.
func repoServer(t *testing.T, repos ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, repo := range repos {
			if r.URL.Path == "/"+repo+"/x86_64/"+repo+".db" {
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedURL is the address of a server that is gone: requests to it get no answer.
func closedURL(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

func pkg(name string) record.Item { return record.Item{Type: record.TypePkg, ID: name, Repo: "extra"} }
func aur(name string) record.Item { return record.Item{Type: record.TypeAUR, ID: name} }

func TestReinstall(t *testing.T) {
	aurPkgs := map[string]map[string]any{
		"paru":         nil,
		"example-bin":  nil,
		"old-tool":     {"Maintainer": nil},
		"stale-widget": {"OutOfDate": 1700000000},
	}
	tests := []struct {
		name  string
		items []record.Item
		sync  []string
		aur   string // "" for the fake AUR, "closed" for one that doesn't answer
		level Level
		text  string
	}{
		{name: "packages available", items: []record.Item{pkg("base"), pkg("htop")}, level: OK, text: "All 2 repository packages are still available"},
		{name: "package gone", items: []record.Item{pkg("base"), pkg("dropped-lib")}, level: Problem, text: "1 of 2 repository packages are no longer"},
		{name: "AUR packages exist", items: []record.Item{aur("paru"), aur("example-bin")}, level: OK, text: "All 2 AUR packages still exist"},
		{name: "AUR package gone", items: []record.Item{aur("paru"), aur("vanished")}, level: Problem, text: "1 of 2 AUR packages are no longer on the AUR"},
		{name: "AUR notes", items: []record.Item{aur("paru"), aur("old-tool"), aur("stale-widget")}, level: Info, text: "old-tool: orphaned"},
		{name: "AUR offline", items: []record.Item{aur("paru")}, aur: "closed", level: Warning, text: "The AUR couldn't be reached"},
		{name: "helper in the list", items: []record.Item{aur("paru"), aur("example-bin")}, level: OK, text: "The AUR helper paru is in the package list"},
		{name: "helper in the repositories", items: []record.Item{aur("example-bin")}, sync: []string{"base", "paru"}, level: OK, text: "paru is in the repositories"},
		{name: "no helper", items: []record.Item{aur("example-bin")}, level: Warning, text: "No AUR helper (paru or yay)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			if tt.sync != nil {
				env.pm.sync = tt.sync
			}
			srv, _ := aurServer(t, aurPkgs)
			opt := env.options()
			opt.AURBase = srv.URL
			if tt.aur == "closed" {
				opt.AURBase = closedURL(t)
			}
			v := env.withItems(opt, tt.items...)
			v.checkReinstall(context.Background())
			l := want(t, v.rep, tt.level, tt.text)
			if tt.name == "package gone" && strings.Join(l.Details, ",") != "dropped-lib" {
				t.Errorf("details = %q", l.Details)
			}
			if tt.name == "AUR package gone" && strings.Join(l.Details, ",") != "vanished" {
				t.Errorf("details = %q", l.Details)
			}
		})
	}
}

func TestSyncListUnreadable(t *testing.T) {
	env := newEnv(t)
	env.pm.syncErr = errors.New("pacman -Slq failed")
	v := env.withItems(env.options(), pkg("base"))
	v.checkReinstall(context.Background())
	want(t, v.rep, Warning, "package lists couldn't be read")
}

func TestAURAsksInBatches(t *testing.T) {
	env := newEnv(t)
	known := map[string]map[string]any{}
	var items []record.Item
	for i := range 250 {
		name := "pkg-" + string(rune('a'+i/26%26)) + string(rune('a'+i%26)) + "-" + string(rune('0'+i/100))
		known[name] = nil
		items = append(items, aur(name))
	}
	srv, queries := aurServer(t, known)
	opt := env.options()
	opt.AURBase = srv.URL
	v := env.withItems(opt, items...)
	v.checkAUR(context.Background(), func() []string {
		var n []string
		for _, it := range items {
			n = append(n, it.ID)
		}
		return n
	}())
	if len(*queries) != 3 {
		t.Errorf("%d requests, want 3", len(*queries))
	}
	want(t, v.rep, OK, "All 250 AUR packages still exist")
}

func TestExtraRepos(t *testing.T) {
	srv := repoServer(t, "myrepo", "included")
	tests := []struct {
		name    string
		section string
		keys    bool
		online  bool // another server answered earlier in the run
		level   Level
		text    string
	}{
		{name: "answers", section: "[myrepo]\nServer = " + srv.URL + "/$repo/$arch\n", level: OK, text: "[myrepo] answers"},
		{name: "second server answers", section: "[myrepo]\nServer = " + srv.URL + "/gone/$arch\nServer = " + srv.URL + "/$repo/$arch\n", level: OK, text: "[myrepo] answers"},
		{name: "server include", section: "[myrepo]\nInclude = /etc/pacman.d/myrepo-mirrorlist\n", level: OK, text: "[myrepo] answers"},
		{name: "not found", section: "[myrepo]\nServer = " + srv.URL + "/elsewhere/$arch\n", level: Problem, text: "[myrepo] doesn't answer"},
		{name: "no server line", section: "[myrepo]\nSigLevel = Optional\n", level: Problem, text: "no Server line"},
		{name: "offline", section: "[myrepo]\nServer = " + closedURL(t) + "/$repo/$arch\n", level: Warning, text: "[myrepo] couldn't be reached"},
		{name: "gone while online", section: "[myrepo]\nServer = " + closedURL(t) + "/$repo/$arch\n", online: true, level: Problem, text: "[myrepo] doesn't answer"},
		{name: "local folder", section: "[myrepo]\nServer = file:///srv/repo/$arch\n", level: OK, text: "[myrepo] answers"},
		{name: "key saved", section: "[myrepo]\nServer = " + srv.URL + "/$repo/$arch\n", keys: true, level: OK, text: "[myrepo]'s signing key is saved"},
		{name: "no key", section: "[myrepo]\nServer = " + srv.URL + "/$repo/$arch\n", level: Warning, text: "No signing key added by hand was found for [myrepo]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			write(t, filepath.Join(env.root, "etc/pacman.d/myrepo-mirrorlist"), "Server = "+srv.URL+"/$repo/$arch\n")
			write(t, filepath.Join(env.root, "srv/repo/x86_64/myrepo.db"), "db")
			it := record.Item{Type: record.TypeRepo, ID: "myrepo", Section: tt.section}
			if tt.keys {
				it.Keys = []string{"0123456789ABCDEF0123456789ABCDEF01234567"}
				write(t, filepath.Join(env.dirs.RecordDir(), capture.RepoKeysFile), "-----BEGIN PGP PUBLIC KEY BLOCK-----\n")
			}
			opt := env.options()
			opt.Arch = "x86_64"
			v := env.withItems(opt, it)
			v.netSeen = tt.online
			v.checkReinstall(context.Background())
			want(t, v.rep, tt.level, tt.text)
		})
	}
}

func TestPacmanArch(t *testing.T) {
	for goarch, want := range map[string]string{"amd64": "x86_64", "arm64": "aarch64", "386": "i686", "riscv64": "riscv64"} {
		if got := pacmanArch(goarch); got != want {
			t.Errorf("pacmanArch(%s) = %s, want %s", goarch, got, want)
		}
	}
}
