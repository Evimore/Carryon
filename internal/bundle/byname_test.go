package bundle

import "testing"

func TestFindsByName(t *testing.T) {
	for _, tc := range []struct {
		folder, pkg string
		want        bool
	}{
		{"/var/lib/jellyfin", "jellyfin", true},
		{"/var/lib/syncthing", "syncthing-bin", true}, // -bin is dropped, as for app bundles
		{"/srv/Plex_Media", "plex-media", true},       // case and punctuation don't count
		{"/srv/media-cache", "plex-media-server", false},
		{"/var/lib/jellyfin", "jellyfin-server", false},
		{"/var/lib/---", "", false},
	} {
		if got := FindsByName(tc.folder, tc.pkg); got != tc.want {
			t.Errorf("FindsByName(%q, %q) = %v, want %v", tc.folder, tc.pkg, got, tc.want)
		}
	}
}
