package pacman

import (
	"context"
	"fmt"

	"github.com/Evimore/carryon/internal/pkgmgr"
)

// installArgs builds pacman's arguments for installing pkgs. With Upgrade it's one
// `pacman -Syu --needed`, because Arch only supports installing on an up-to-date system.
func installArgs(pkgs []string, opt pkgmgr.InstallOptions) ([]string, error) {
	if opt.Upgrade && opt.AsDeps {
		return nil, pkgmgr.ErrUpgradeAsDeps
	}
	args := []string{"-S"}
	if opt.Upgrade {
		args = []string{"-Syu"}
	}
	args = append(args, "--needed")
	if opt.AsDeps {
		args = append(args, "--asdeps")
	}
	if opt.NoConfirm {
		args = append(args, "--noconfirm")
	}
	return append(args, pkgs...), nil
}

// eachArgs builds pacman's arguments for installing one package without asking.
func eachArgs(pkg string, asDeps bool) []string {
	args := []string{"-S", "--needed", "--noconfirm"}
	if asDeps {
		args = append(args, "--asdeps")
	}
	return append(args, pkg)
}

// Install installs packages in one pacman run attached to the terminal. With nothing to install
// and no upgrade asked for, it does nothing.
func (p *Pacman) Install(ctx context.Context, pkgs []string, opt pkgmgr.InstallOptions) error {
	if len(pkgs) == 0 && !opt.Upgrade {
		return nil
	}
	args, err := installArgs(pkgs, opt)
	if err != nil {
		return err
	}
	if err := p.run.Run(ctx, "pacman", args...); err != nil {
		return fmt.Errorf("installing %d packages with pacman: %w", len(pkgs), err)
	}
	return nil
}

// InstallEach installs packages one at a time without asking, skipping any that fail, and returns
// those. Like the prototype's install_one_by_one; asking the user first is up to the caller.
func (p *Pacman) InstallEach(ctx context.Context, pkgs []string, asDeps bool) []string {
	var failed []string
	for _, pkg := range pkgs {
		if err := p.run.Run(ctx, "pacman", eachArgs(pkg, asDeps)...); err != nil {
			failed = append(failed, pkg)
		}
	}
	return failed
}
