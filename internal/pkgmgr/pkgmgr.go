// Package pkgmgr describes what Carryon needs from a package manager: which packages the user chose,
// which files packages own and which of those were changed, extra repositories and their signing
// keys, and installing packages on restore. See docs/design.md, "What gets captured".
//
// The pacman backend lives in internal/pkgmgr/pacman. The interface is written so apt and dnf
// backends can follow.
package pkgmgr

import (
	"context"
	"errors"
)

// Runner runs external programs. internal/sysexec provides the real one and a fake for tests.
type Runner interface {
	// Output runs a program quietly and returns what it printed on stdout. When the program ran
	// but exited non-zero, the error has an ExitCode() int method and the output is still returned.
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
	// Run runs a program attached to the terminal, so the user sees its prompts.
	Run(ctx context.Context, name string, args ...string) error
	// Exists reports whether a program is installed.
	Exists(name string) bool
}

// UnknownRepo is the repository recorded for a package no configured repository has (format 1
// records use "?").
const UnknownRepo = "?"

// Package is an installed package the user chose, or one that's only optionally needed.
type Package struct {
	Name    string
	Repo    string // repository it comes from; UnknownRepo when none has it; "" for foreign packages
	Foreign bool   // not from a repository: AUR or built by hand
	AsDeps  bool   // installed as a dependency, but only optionally needed by what's installed
}

// Repo is an extra package repository: one that isn't part of the distribution.
type Repo struct {
	Name    string
	Conf    string // absolute path of the file holding its section; "" for the main config file
	Section string // the section's text, ending in a newline; "" when it wasn't found
}

// Key is a signing key the user added by hand.
type Key struct {
	Fingerprint string
	UID         string // the key's first user ID, e.g. "Example Repo <repo@example.com>"
}

// EditedFile is a package file whose contents differ from the package, other than a config file
// the package expects to be changed.
type EditedFile struct {
	Path    string
	Package string
	Reasons []string // what differs, as the package manager says it, e.g. "Size mismatch"
}

// InstallOptions change how packages are installed.
type InstallOptions struct {
	Upgrade   bool // refresh the package lists and upgrade the system in the same run
	AsDeps    bool // mark the packages as installed as dependencies
	NoConfirm bool // don't let the package manager ask for confirmation
}

// ErrUpgradeAsDeps is returned when Upgrade and AsDeps are both asked for: marking a whole upgrade
// as dependencies would change the install reason of every upgraded package.
var ErrUpgradeAsDeps = errors.New("can't upgrade the system and mark packages as dependencies in one run")

// Manager is a package manager backend.
type Manager interface {
	// Name is the package manager's name, e.g. "pacman".
	Name() string
	// Present reports whether the package manager is installed on this machine.
	Present() bool

	// Packages lists the packages the user installed on purpose, then the ones installed as
	// dependencies that are only optionally needed. Leftovers nothing uses aren't listed.
	Packages(ctx context.Context) ([]Package, error)
	// Installed lists every installed package's name.
	Installed(ctx context.Context) ([]string, error)
	// Versions lists every installed package with its version, one per line, for reference.
	Versions(ctx context.Context) ([]byte, error)
	// SyncPackages lists the packages the configured repositories offer.
	SyncPackages(ctx context.Context) ([]string, error)

	// OwnedFiles lists every path an installed package owns, without trailing slashes.
	OwnedFiles(ctx context.Context) ([]string, error)
	// FilesOf lists the paths each of the given packages owns.
	FilesOf(ctx context.Context, pkgs ...string) (map[string][]string, error)
	// Owners maps each path that a package owns to that package. Keys are the paths as given,
	// even when the package manager knows them by another name (/lib is /usr/lib).
	Owners(ctx context.Context, paths []string) (map[string]string, error)
	// ModifiedConfigs lists the config files packages expect to be changed (pacman's backup=
	// files) that differ from the package.
	ModifiedConfigs(ctx context.Context) ([]string, error)
	// EditedFiles lists the other package files whose contents differ from the package. It reads
	// every package file, so it's slow: meant for the nightly capture only.
	EditedFiles(ctx context.Context) ([]EditedFile, error)

	// ConfiguredRepos lists the repositories in the package manager's config, in order.
	ConfiguredRepos(ctx context.Context) ([]string, error)
	// ExtraRepos lists the configured repositories that aren't the distribution's.
	ExtraRepos(ctx context.Context) ([]Repo, error)
	// ExtraKeys lists the signing keys the user added by hand.
	ExtraKeys(ctx context.Context) ([]Key, error)
	// ExportKeys returns the given keys, ASCII-armored.
	ExportKeys(ctx context.Context, fingerprints []string) ([]byte, error)
	// TrustKeys imports the keys in an armored key file and trusts the given ones. It returns the
	// fingerprints it couldn't trust.
	TrustKeys(ctx context.Context, keyFile string, fingerprints []string) ([]string, error)

	// Install installs packages in one run, attached to the terminal.
	Install(ctx context.Context, pkgs []string, opt InstallOptions) error
	// InstallEach installs packages one at a time without asking, attached to the terminal, and
	// returns the ones that failed. It's the fallback when Install fails.
	InstallEach(ctx context.Context, pkgs []string, asDeps bool) []string
}
