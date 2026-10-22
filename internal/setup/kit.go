package setup

import (
	"fmt"
	"io"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/ui"
)

// Kit is what someone needs to get back in on a new machine.
type Kit struct {
	URL             string
	MachineName     string
	Password        string // empty when PasswordCommand gives it
	PasswordCommand string
	RcloneUser      string
}

// LoadKit builds the kit from the config, reading the password from its file.
func LoadKit(cfg config.Config, passwordFile string) (Kit, error) {
	k := Kit{
		URL:             cfg.Repository.URL,
		MachineName:     cfg.MachineName,
		PasswordCommand: cfg.Repository.PasswordCommand,
		RcloneUser:      cfg.Repository.RcloneUser,
	}
	if k.PasswordCommand == "" {
		if cfg.Repository.PasswordFile != "" {
			passwordFile = cfg.Repository.PasswordFile
		}
		pw, err := ReadPasswordFile(passwordFile)
		if err != nil {
			return Kit{}, err
		}
		k.Password = pw
	}
	return k, nil
}

// Write prints the kit.
func (k Kit) Write(w io.Writer) {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("")
	p("Carryon recovery kit")
	p("====================")
	p("")
	p("Keep this outside this machine, for example in your password manager. Without the")
	p("password nobody can open the backup, including you.")
	p("")
	p("  Repository:    %s", k.URL)
	p("  Machine name:  %s", k.MachineName)
	if k.Password != "" {
		p("  Password:      %s", k.Password)
	} else {
		p("  Password:      Carryon reads it with: %s", k.PasswordCommand)
		p("                 Save the password itself too: that command may only work on this machine.")
	}
	if strings.HasPrefix(k.URL, "rclone:") {
		remote, err := dest.RcloneRemote(k.URL)
		if err != nil {
			remote = "the rclone remote"
		}
		who := "your"
		if k.RcloneUser != "" {
			who = k.RcloneUser + "'s"
		}
		p("  rclone login:  %s rclone remote %s (set it up again with: rclone config)", who, remote)
	}
	p("")
	p("On a new machine:")
	p("  1. Install your system and Carryon (from the AUR: carryon).")
	if strings.HasPrefix(k.URL, "rclone:") {
		p("  2. As yourself, set up the same rclone remote: rclone config")
	} else {
		p("  2. Make the destination reachable (plug in and mount the drive, or get online).")
	}
	p("  3. Run: sudo carryon init --connect %s", quote(k.URL))
	p("     and enter the password. Automatic backups pause until the restore is done.")
	p("  4. Run: sudo carryon restore")
	p("     ideally from a text console (Ctrl+Alt+F3) while signed out of the desktop.")
	p("")
	p("Without Carryon, with plain restic (it asks for the password):")
	if strings.HasPrefix(k.URL, "rclone:") {
		p("  (run these as the user whose rclone login reaches the remote)")
	}
	p("  restic -r %s snapshots --host %s", quote(k.URL), quote(k.MachineName))
	p("  restic -r %s restore latest --host %s --target /tmp/restored --include /home/yourname/Documents", quote(k.URL), quote(k.MachineName))
	p("  The files land under /tmp/restored. Leave out --include to get everything back.")
	p("")
	io.WriteString(w, b.String())
}

// saveKitSoon is the reminder for a kit the user hasn't confirmed as saved.
const saveKitSoon = "Save the recovery kit soon: \"sudo carryon recovery-kit\" shows it again, and \"carryon verify\" reminds you until you confirm."

// showKit prints the kit and asks the user to confirm they saved it, unless they already did.
// saved answers the question without asking (--kit-saved).
func (e *Env) showKit(k Kit, saved bool) error {
	k.Write(e.Out)
	if KitConfirmed(e.Dirs) {
		return nil
	}
	if !saved {
		var err error
		saved, err = e.Prompt.Confirm(ui.Confirm{
			Title:   "Have you saved the recovery kit?",
			Text:    "Save the repository, machine name and password above somewhere outside this machine.",
			Default: false,
			Flag:    "--kit-saved",
		})
		if err != nil {
			return err
		}
	}
	if !saved {
		e.Log.Warn("%s", saveKitSoon)
		return nil
	}
	if err := e.markKitConfirmed(); err != nil {
		return err
	}
	e.Log.Info("Noted that you saved the recovery kit.")
	return nil
}

// RecoveryKit shows the kit of the machine set up in cfg, and asks the user to confirm they saved
// it if they haven't yet.
func (e *Env) RecoveryKit(cfg config.Config, saved bool) error {
	k, err := LoadKit(cfg, e.Dirs.PasswordFile())
	if err != nil {
		return err
	}
	return e.showKit(k, saved)
}
