package state

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEntryID(t *testing.T) {
	// Systemd instance names allow letters, digits and ":_.-\"; IDs keep to the safe part.
	safe := regexp.MustCompile(`^[a-z0-9-]{1,30}$`)
	for _, tc := range []struct{ kind, target string }{
		{KindPath, "/var/lib/jellyfin"},
		{KindPath, "/srv/my files/with spaces"},
		{KindRestore, "app-obs-studio"},
	} {
		id := EntryID(tc.kind, tc.target)
		if !safe.MatchString(id) || !ValidEntryID(id) {
			t.Errorf("EntryID(%q, %q) = %q, not a safe instance name", tc.kind, tc.target, id)
		}
		if again := EntryID(tc.kind, tc.target); again != id {
			t.Errorf("EntryID isn't stable: %q then %q", id, again)
		}
	}
	if EntryID(KindPath, "/var/lib/foo/") != EntryID(KindPath, "/var/lib/foo") {
		t.Error("a trailing slash changes the ID")
	}
	if EntryID(KindPath, "/var/lib/a") == EntryID(KindPath, "/var/lib/b") {
		t.Error("two paths share an ID")
	}
	if EntryID(KindPath, "app-x") == EntryID(KindRestore, "app-x") {
		t.Error("two kinds share an ID")
	}
	for _, bad := range []string{"", "/var/lib/foo", "path-XYZ", "path-3fa9c2d1e0/../x", "other-3fa9c2d1e0"} {
		if ValidEntryID(bad) {
			t.Errorf("ValidEntryID(%q) = true", bad)
		}
	}
}

func TestInboxPutFindRemove(t *testing.T) {
	d := testDirs(t)
	in, err := ReadInbox(d)
	if err != nil || len(in.Entries) != 0 {
		t.Fatalf("empty inbox: %+v, %v", in, err)
	}
	first := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	e := NewEntry(KindPath, "/var/lib/jellyfin")
	e.Found, e.By, e.Size, e.App, e.Bundle = first, ByHook, 1024, "jellyfin-server", "app-jellyfin-server"
	if _, err := UpdateInbox(d, InboxWait, func(in *Inbox) error {
		if !in.Put(e) {
			t.Error("Put of a new entry says it was there")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Found again by the nightly backup, bigger now: replaced, first finding kept.
	again := e
	again.Found, again.By, again.Size, again.Large = first.Add(24*time.Hour), ByBackup, 9<<30, true
	if _, err := UpdateInbox(d, InboxWait, func(in *Inbox) error {
		if in.Put(again) {
			t.Error("Put of a known entry says it is new")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in, err = ReadInbox(d)
	if err != nil || len(in.Entries) != 1 {
		t.Fatalf("inbox = %+v, %v", in, err)
	}
	got := in.Entries[0]
	if !got.Found.Equal(first) || got.By != ByHook || got.Size != 9<<30 || !got.Large {
		t.Errorf("replaced entry = %+v", got)
	}
	for _, arg := range []string{e.ID, "/var/lib/jellyfin", "/var/lib/jellyfin/"} {
		if f, ok := in.Find(arg); !ok || f.ID != e.ID {
			t.Errorf("Find(%q) = %+v, %v", arg, f, ok)
		}
	}
	if _, ok := in.Find("var/lib/jellyfin"); ok {
		t.Error("Find took a relative path")
	}
	if InboxCount(d) != 1 {
		t.Errorf("InboxCount = %d", InboxCount(d))
	}

	if _, err := UpdateInbox(d, InboxWait, func(in *Inbox) error {
		if _, ok := in.Remove(e.ID); !ok {
			t.Error("Remove found nothing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if InboxCount(d) != 0 {
		t.Errorf("InboxCount after Remove = %d", InboxCount(d))
	}
}

func TestInboxIsWorldReadable(t *testing.T) {
	d := testDirs(t)
	if _, err := UpdateInbox(d, InboxWait, func(in *Inbox) error {
		in.Put(NewEntry(KindRestore, "app-obs-studio"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(d.InboxFile())
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, %v", fi.Mode(), err)
	}
}

func TestInboxFailedChangeWritesNothing(t *testing.T) {
	d := testDirs(t)
	boom := errors.New("boom")
	if _, err := UpdateInbox(d, InboxWait, func(in *Inbox) error {
		in.Put(NewEntry(KindPath, "/srv/data"))
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(d.InboxFile()); !os.IsNotExist(err) {
		t.Errorf("the inbox was written: %v", err)
	}
}

func TestDamagedInboxIsSetAside(t *testing.T) {
	d := testDirs(t)
	if err := os.MkdirAll(d.State, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.InboxFile(), []byte(`{"entries": [`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInbox(d); !errors.Is(err, ErrInboxDamaged) {
		t.Fatalf("ReadInbox: %v", err)
	}
	setAside, err := UpdateInbox(d, InboxWait, func(in *Inbox) error {
		in.Put(NewEntry(KindPath, "/srv/data"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if setAside != d.InboxFile()+".damaged" {
		t.Errorf("setAside = %q", setAside)
	}
	if data, err := os.ReadFile(setAside); err != nil || string(data) != `{"entries": [` {
		t.Errorf("set-aside copy = %q, %v", data, err)
	}
	if InboxCount(d) != 1 {
		t.Errorf("InboxCount = %d", InboxCount(d))
	}
}

func TestInboxWritersAtTheSameTimeKeepEveryEntry(t *testing.T) {
	d := testDirs(t)
	const writers = 40
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := UpdateInbox(d, InboxWait, func(in *Inbox) error {
				in.Put(NewEntry(KindPath, fmt.Sprintf("/var/lib/app%d", i)))
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	in, err := ReadInbox(d)
	if err != nil || len(in.Entries) != writers {
		t.Fatalf("%d entries, want %d (%v)", len(in.Entries), writers, err)
	}
}

func TestInboxWriterGivesUpOnABusyLock(t *testing.T) {
	d := testDirs(t)
	unlock, err := lockInbox(d, 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = UpdateInbox(d, 50*time.Millisecond, func(*Inbox) error {
		t.Error("change ran without the lock")
		return nil
	})
	if !errors.Is(err, ErrInboxBusy) {
		t.Fatalf("err = %v, want ErrInboxBusy", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("waited %v", waited)
	}
	unlock()
	if _, err := UpdateInbox(d, 0, func(*Inbox) error { return nil }); err != nil {
		t.Errorf("after unlock: %v", err)
	}
}

func TestEntryFieldsAreInAlphabeticalOrder(t *testing.T) {
	typ := reflect.TypeFor[Entry]()
	prev := ""
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name < prev {
			t.Errorf("%s comes after %s", name, prev)
		}
		prev = name
	}
}
