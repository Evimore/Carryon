package pacman

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Evimore/carryon/internal/pkgmgr"
)

// keyringDir is pacman's keyring, and distroKeyrings the keyrings the distribution's keyring
// packages install.
const (
	keyringDir     = "/etc/pacman.d/gnupg"
	distroKeyrings = "/usr/share/pacman/keyrings/*.gpg"
)

// parseGPGKeys reads `gpg --with-colons` key listings into (fingerprint, first user ID) pairs. A
// port of the prototype's parse_gpg_keys: the fingerprint is the first fpr record after a pub
// record (the primary key's; subkeys' come later), and the user ID the first uid record after it.
func parseGPGKeys(listing []byte) []pkgmgr.Key {
	var keys []pkgmgr.Key
	state, fpr := "", ""
	for _, line := range strings.Split(string(listing), "\n") {
		f := strings.Split(line, ":")
		switch {
		case f[0] == "pub":
			state, fpr = "pub", ""
		case f[0] == "fpr" && state == "pub" && fpr == "" && len(f) > 9:
			fpr = f[9]
		case f[0] == "uid" && state == "pub" && fpr != "" && len(f) > 9:
			keys = append(keys, pkgmgr.Key{Fingerprint: fpr, UID: f[9]})
			state = ""
		}
	}
	return keys
}

// gpg returns the arguments for gpg working on pacman's keyring.
func (p *Pacman) gpg(args ...string) []string {
	return append([]string{"--homedir", p.path(keyringDir), "--no-permission-warning", "--batch",
		"--no-auto-check-trustdb"}, args...)
}

// ExtraKeys lists the signing keys added to pacman's keyring by hand: the keyring minus the keys in
// the distribution's keyrings (/usr/share/pacman/keyrings/*.gpg) and minus pacman's own local key
// (pacman@localhost). Like the prototype's capture_repos.
func (p *Pacman) ExtraKeys(ctx context.Context) ([]pkgmgr.Key, error) {
	out, err := p.run.Output(ctx, "gpg", p.gpg("--with-colons", "--list-keys")...)
	if err != nil && (exitCode(err) <= 0 || len(out) == 0) {
		return nil, fmt.Errorf("reading pacman's keyring: %w", err)
	}
	mine := parseGPGKeys(out)
	if len(mine) == 0 {
		return nil, nil
	}

	// --show-keys still wants a home folder; a scratch one keeps pacman's keyring untouched.
	scratch, err := os.MkdirTemp(p.opt.TempDir, "carryon-gnupg-")
	if err != nil {
		return nil, fmt.Errorf("making a scratch folder for gpg: %w", err)
	}
	defer os.RemoveAll(scratch)
	keyrings, err := filepath.Glob(p.path(distroKeyrings))
	if err != nil {
		return nil, err
	}
	slices.Sort(keyrings)
	distro := map[string]bool{}
	for _, kr := range keyrings {
		out, err := p.run.Output(ctx, "gpg", "--homedir", scratch, "--batch", "--with-colons", "--show-keys", kr)
		if err != nil && (exitCode(err) <= 0 || len(out) == 0) {
			return nil, fmt.Errorf("reading the keyring %s: %w", kr, err)
		}
		for _, k := range parseGPGKeys(out) {
			distro[k.Fingerprint] = true
		}
	}

	var extra []pkgmgr.Key
	for _, k := range mine {
		if !distro[k.Fingerprint] && !strings.Contains(k.UID, "pacman@localhost") {
			extra = append(extra, k)
		}
	}
	return extra, nil
}

// ExportKeys returns the given keys from pacman's keyring, ASCII-armored.
func (p *Pacman) ExportKeys(ctx context.Context, fingerprints []string) ([]byte, error) {
	if len(fingerprints) == 0 {
		return nil, nil
	}
	out, err := p.run.Output(ctx, "gpg", p.gpg(append([]string{"--armor", "--export"}, fingerprints...)...)...)
	if err != nil {
		return nil, fmt.Errorf("exporting signing keys from pacman's keyring: %w", err)
	}
	return out, nil
}

// TrustKeys adds the keys in an armored key file to pacman's keyring and signs the given ones
// locally, so packages signed with them install. It returns the keys it couldn't trust.
func (p *Pacman) TrustKeys(ctx context.Context, keyFile string, fingerprints []string) ([]string, error) {
	if err := p.run.Run(ctx, "pacman-key", "--add", keyFile); err != nil {
		return fingerprints, fmt.Errorf("adding signing keys from %s: %w", keyFile, err)
	}
	var failed []string
	for _, k := range fingerprints {
		if err := p.run.Run(ctx, "pacman-key", "--lsign-key", k); err != nil {
			failed = append(failed, k)
		}
	}
	return failed, nil
}
