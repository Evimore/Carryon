package record

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadPrototypeRecord(t *testing.T) {
	r, err := Load("testdata/format1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Meta.Format != FormatPrototype || r.Meta.Name() != "desktop" {
		t.Errorf("meta = %+v", r.Meta)
	}
	if users := r.Meta.AllUsers(); len(users) != 1 || users[0] != (User{Name: "alice", Home: "/home/alice"}) {
		t.Errorf("AllUsers() = %+v", users)
	}
	if len(r.Items) != 18 {
		t.Fatalf("got %d items, want 18", len(r.Items))
	}
	if len(r.Bundles) != 14 || r.Bundles[0].ID != "packages" || r.Bundles[len(r.Bundles)-1].ID != "home-apps" {
		t.Errorf("bundles out of order: %+v", r.Bundles)
	}
	if b, ok := r.Bundles.Get("identity"); !ok || b.When != "samehost" {
		t.Errorf("identity bundle = %+v, %v", b, ok)
	}

	root := find(t, r.Items, "path:/root")
	if root.UID == nil || *root.UID != 0 || root.GID == nil || *root.GID != 0 {
		t.Errorf("/root lost its numeric owner: uid=%v gid=%v", root.UID, root.GID)
	}
	repo := find(t, r.Items, "repo:myrepo")
	if !strings.Contains(repo.Section, "Server = https://repo.example.com/$arch") || len(repo.Keys) != 1 {
		t.Errorf("repo = %+v", repo)
	}
	if fuse := find(t, r.Items, "pkg:fuse3"); !fuse.AsDeps || fuse.ForUnit != "rclone-mount.service" {
		t.Errorf("fuse3 = %+v", fuse)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	r, err := Load("testdata/format1")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := r.Save(dir); err != nil {
		t.Fatal(err)
	}
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, again) {
		t.Fatalf("round trip changed the record:\nbefore %+v\nafter  %+v", r, again)
	}
}

func TestItemsAreWrittenWithSortedKeys(t *testing.T) {
	zero := 0
	var b bytes.Buffer
	if err := WriteItems(&b, []Item{{Type: TypePath, ID: "/root", Bundle: "root-home", Owner: "root:root", UID: &zero, GID: &zero}}); err != nil {
		t.Fatal(err)
	}
	want := `{"bundle":"root-home","gid":0,"id":"/root","owner":"root:root","type":"path","uid":0}` + "\n"
	if b.String() != want {
		t.Fatalf("got  %s want %s", b.String(), want)
	}
}

func TestItemKeysAreSortedInDeclaration(t *testing.T) {
	// Item's fields must stay in alphabetical order of their JSON names (see the type's comment).
	typ := reflect.TypeOf(Item{})
	prev := ""
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name < prev {
			t.Errorf("field %s (json %q) comes after %q: keep Item's fields sorted by JSON name", typ.Field(i).Name, name, prev)
		}
		prev = name
	}
}

func TestReadItemsRejectsBrokenLines(t *testing.T) {
	_, err := ReadItems(strings.NewReader("{\"type\":\"pkg\",\"id\":\"a\"}\n\n{\"type\":\"pkg\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("err = %v, want an error about line 3", err)
	}
}

func TestLoadRejectsUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	meta, _ := json.Marshal(Meta{Format: 99})
	if err := os.WriteFile(filepath.Join(dir, MetaFile), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "format 99") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiff(t *testing.T) {
	old := []Item{
		{Type: TypePkg, ID: "vim", Repo: "extra"},
		{Type: TypePkg, ID: "nano", Repo: "core"},
		{Type: TypeFile, ID: "/etc/hosts", SHA: "a"},
	}
	new := []Item{
		{Type: TypePkg, ID: "vim", Repo: "extra"},
		{Type: TypeFile, ID: "/etc/hosts", SHA: "b"},
		{Type: TypeAUR, ID: "paru-bin"},
	}
	added, removed, changed := Diff(old, new)
	got := ChangeLines(added, removed, changed)
	want := []string{
		"+ AUR        paru-bin",
		"- package    nano  [core]",
		"~ file       /etc/hosts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangeLines = %q, want %q", got, want)
	}
}

func TestDiffIgnoresBundleMoves(t *testing.T) {
	a := []Item{{Type: TypePkg, ID: "vim", Bundle: "packages"}}
	b := []Item{{Type: TypePkg, ID: "vim", Bundle: "app-vim"}}
	if added, removed, changed := Diff(a, b); len(added)+len(removed)+len(changed) != 0 {
		t.Fatalf("a bundle move isn't a change: %v %v %v", added, removed, changed)
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Item{
		"package    vim  [extra]  (as dependency)": {Type: TypePkg, ID: "vim", Repo: "extra", AsDeps: true},
		"package    vim": {Type: TypePkg, ID: "vim", Repo: "?"},
		"repo       [myrepo] + signing key (Example <a@example.com>)": {Type: TypeRepo, ID: "myrepo", Keys: []string{"F"}, KeyNames: []string{"Example <a@example.com>"}},
		"file       /etc/ssh/sshd_config  (edited package file)":      {Type: TypeFile, ID: "/etc/ssh/sshd_config", Kind: KindModified},
		"backup     ~/.config/kitty":                                  {Type: TypePath, ID: "/home/alice/.config/kitty", Home: ".config/kitty"},
		"folder     ~/Cloud":                                          {Type: TypeMkdir, ID: "/home/alice/Cloud", Rel: "Cloud"},
		"user unit  enable rclone-mount.service":                      {Type: TypeUsvc, ID: "rclone-mount.service"},
		"npm -g     typescript":                                       {Type: TypeNpm, ID: "typescript"},
		"newtype x":                                                   {Type: "newtype", ID: "x"},
	}
	for want, it := range cases {
		if got := it.Describe(); got != want {
			t.Errorf("Describe(%+v) = %q, want %q", it, got, want)
		}
	}
}

func TestBrief(t *testing.T) {
	if got := Brief([]string{"a", "b", "c"}, 2); got != "a, b ... (+1 more)" {
		t.Errorf("Brief = %q", got)
	}
	if got := Brief([]string{"a", "b"}, 2); got != "a, b" {
		t.Errorf("Brief = %q", got)
	}
}

func TestBundlesRejectDuplicates(t *testing.T) {
	var bs Bundles
	err := json.Unmarshal([]byte(`{"a":{"title":"A","when":"always"},"a":{"title":"B","when":"never"}}`), &bs)
	if err == nil {
		t.Fatal("want an error for a duplicate bundle id")
	}
}

func find(t *testing.T, items []Item, key string) Item {
	t.Helper()
	for _, it := range items {
		if it.Key() == key {
			return it
		}
	}
	t.Fatalf("no item %s", key)
	return Item{}
}
