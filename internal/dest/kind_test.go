package dest

import "testing"

func TestKind(t *testing.T) {
	for fstype, want := range map[string]Kind{
		"ext4": Local, "btrfs": Local, "vfat": Local, "tmpfs": Local, "fuseblk": Local,
		"fuse": FUSE, "fuse.rclone": FUSE, "fuse.sshfs": FUSE, "fuse.gvfsd-fuse": FUSE,
		"nfs": Network, "nfs4": Network, "cifs": Network, "smb3": Network, "9p": Network,
	} {
		if got := (Mount{FSType: fstype}).Kind(); got != want {
			t.Errorf("%s: got %v, want %v", fstype, got, want)
		}
	}
}

func TestOnRemoteMount(t *testing.T) {
	mounts, err := ReadMounts("testdata/remote")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/home/alice/iCloud/carryon/carryon":  "/home/alice/iCloud",
		"/home/alice/My Drive/config.toml":    "/home/alice/My Drive",
		"/mnt/nas/backup":                     "/mnt/nas",
		"/run/user/1000/gvfs/smb-share:x/dir": "/run/user/1000/gvfs",
		"/home/alice/Media":                   "/home/alice/Media",
		"/home/alice/bin/carryon":             "",
		"/usr/bin/carryon":                    "",
		"/mnt/windows/backup":                 "", // fuseblk is a local disk
	} {
		m, ok := mounts.OnRemoteMount(path)
		if ok != (want != "") || m.MountPoint != want {
			t.Errorf("OnRemoteMount(%q) = %q, %v; want %q", path, m.MountPoint, ok, want)
		}
	}
}

func TestDirectBackend(t *testing.T) {
	mounts, err := ReadMounts("testdata/remote")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/home/alice/iCloud":                "rclone:iCloud:",
		"/home/alice/iCloud/Backups/laptop": "rclone:iCloud:Backups/laptop",
		"/home/alice/My Drive/carryon":      "rclone:gdrive:shared/team/carryon",
		"/home/alice/server":                "sftp:alice@example.com:/srv/backup",
		"/home/alice/server/desktop/":       "sftp:alice@example.com:/srv/backup/desktop",
		"/home/alice/home-on-server/repo":   "sftp:alice@example.com:repo",
		"/home/alice/home-on-server":        "", // sftp: needs a path
		"/mnt/nas/backup":                   "", // NFS: no direct backend Carryon knows
		"/run/user/1000/gvfs/x":             "",
		"/home/alice/Documents":             "",
	} {
		got, ok := mounts.DirectBackend(path)
		if got != want || ok != (want != "") {
			t.Errorf("DirectBackend(%q) = %q, %v; want %q", path, got, ok, want)
		}
	}
}

func TestDirectBackendBindMount(t *testing.T) {
	mounts := MountTable{
		{MountPoint: "/"},
		{MountPoint: "/srv/backup", Root: "/carryon", FSType: "fuse.rclone", Source: "b2remote:bucket"},
	}
	if got, _ := mounts.DirectBackend("/srv/backup/desktop"); got != "rclone:b2remote:bucket/carryon/desktop" {
		t.Errorf("got %q", got)
	}
}
