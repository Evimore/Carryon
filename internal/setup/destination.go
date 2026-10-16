package setup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/ui"
)

// repoSchemes are the restic URL prefixes Carryon accepts besides a local folder.
var repoSchemes = []string{"sftp:", "rest:", "s3:", "b2:", "azure:", "gs:", "swift:", "rclone:"}

// normalizeURL checks a repository URL the user gave and returns it in the form the config keeps:
// a local folder as a clean absolute path, anything else unchanged.
func normalizeURL(url string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", errors.New("the destination is empty")
	}
	if dest.IsLocal(url) {
		p := dest.LocalPath(url)
		if strings.HasPrefix(p, "~") {
			return "", fmt.Errorf("%q: write the folder's full path, such as /home/alice/backup (init runs as root, so ~ would be root's home)", url)
		}
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%q: use a folder's full path starting with /, or a restic URL (%s)", url, strings.Join(repoSchemes, " "))
		}
		return filepath.Clean(p), nil
	}
	if strings.ContainsAny(url, "\n\r") {
		return "", errors.New("the destination must fit on one line")
	}
	return url, nil
}

// mountDirs are where drives usually get mounted. A destination there that sits on the system
// disk most likely means the drive isn't mounted.
var mountDirs = []string{"/mnt", "/media", "/run/media"}

// guardDestination checks a destination before anything is written there and returns the URL to
// use. A local folder on a FUSE or network mount gets the direct backend offered, and a folder that
// looks like an empty mount point needs confirming. yes accepts the risks without asking.
func (e *Env) guardDestination(url string, yes bool) (string, error) {
	if !dest.IsLocal(url) {
		return url, nil
	}
	mounts, err := dest.ReadMounts(e.root())
	if err != nil {
		return "", fmt.Errorf("Carryon can't tell which drive %s is on: %w", url, err)
	}
	resolved, err := dest.ResolvePath(e.root(), url)
	if err != nil {
		return "", fmt.Errorf("following the path %s: %w", url, err)
	}
	if m, remote := mounts.OnRemoteMount(resolved); remote {
		return e.remoteMountDestination(url, resolved, mounts, m, yes)
	}
	m, ok := mounts.Containing(resolved)
	if !ok {
		return "", fmt.Errorf("no filesystem is mounted at %s or above it", url)
	}
	if m.MountPoint != "/" {
		return url, nil
	}
	for _, d := range mountDirs {
		if resolved == d || strings.HasPrefix(resolved, d+"/") {
			ok, err := e.confirmRisk(yes, ui.Confirm{
				Title: "Back up to the system disk?",
				Text: fmt.Sprintf("No drive is mounted at %s: it is a folder on the system disk. If the backup drive "+
					"isn't plugged in, mount it first and run init again. Backing up here fills the system disk, and the "+
					"backup is lost with it.", url),
			})
			if err != nil {
				return "", err
			}
			if !ok {
				return "", fmt.Errorf("nothing was set up: mount the backup drive at %s, or choose another destination with --repo", url)
			}
			return url, nil
		}
	}
	e.Log.Warn("%s is on the system disk, so the backup won't survive that disk failing. A second drive or a remote destination is safer.", url)
	return url, nil
}

// remoteMountDestination handles a local folder on a FUSE or network mount: root often can't read
// such a mount, the root service starts before it exists, and some report a write as done before
// it's uploaded.
func (e *Env) remoteMountDestination(url, resolved string, mounts dest.MountTable, m dest.Mount, yes bool) (string, error) {
	why := fmt.Sprintf("%s is on a %s mount (%s at %s). Backups run as root when the system starts, often before "+
		"the mount exists and without access to it, and some mounts say a file is saved before it's uploaded.",
		url, m.Kind(), orDefault(m.FSType, "unknown type"), m.MountPoint)
	if direct, ok := mounts.DirectBackend(resolved); ok {
		use, err := e.Prompt.Confirm(ui.Confirm{
			Title:   "Connect directly instead of through the mount?",
			Text:    why + "\nCarryon can reach the same place directly as " + direct + ", without the mount.",
			Default: true,
		})
		if err != nil {
			return "", err
		}
		if use {
			e.Log.Info("Using %s instead of the mount.", direct)
			return direct, nil
		}
	} else {
		e.Log.Warn("%s", why)
	}
	ok, err := e.confirmRisk(yes, ui.Confirm{
		Title: "Back up through the mount anyway?",
		Text:  "Backups may fail when the mount isn't there, or look saved before they are. A restic URL such as sftp: or rclone: avoids that.",
	})
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("nothing was set up: choose a destination that isn't on the %s mount at %s with --repo, for example an sftp: or rclone: URL", m.Kind(), m.MountPoint)
	}
	return url, nil
}

// confirmRisk asks the user to accept a risk. It never accepts on its own: without a terminal it
// needs --yes.
func (e *Env) confirmRisk(yes bool, c ui.Confirm) (bool, error) {
	if yes {
		e.Log.Warn("%s Going ahead, because of --yes.", c.Text)
		return true, nil
	}
	c.Default, c.NoDefault, c.Flag = false, true, "--yes"
	return e.Prompt.Confirm(c)
}

// recordDestination works out the destination's identity for backups to check, once the repository
// exists: for a local folder its drive and the repository ID, otherwise the repository ID.
func (e *Env) recordDestination(ctx context.Context, url string, repo dest.Repository) (dest.Identity, error) {
	if dest.IsLocal(url) {
		return dest.Record(ctx, e.root(), dest.LocalPath(url), repo)
	}
	id, err := repo.RepositoryID(ctx)
	if err != nil {
		return dest.Identity{}, fmt.Errorf("reading the repository ID: %w", err)
	}
	return dest.Identity{RepoID: id}, nil
}
