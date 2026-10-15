// Command example tries every prompt in internal/ui, for reviewing the terminal UI by hand:
//
//	go run ./internal/ui/example                 # full-screen checklist and forms
//	go run ./internal/ui/example -accessible     # plain line-by-line prompts (also TERM=dumb)
//	go run ./internal/ui/example -defaults       # no questions: the defaults, or an error
//	NO_COLOR=1 go run ./internal/ui/example      # no colours
//	go run ./internal/ui/example < /dev/null     # stdin isn't a terminal: same as -defaults
//
// It changes nothing; it prints the answers.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Evimore/carryon/internal/ui"
)

func main() {
	defaults := flag.Bool("defaults", false, "answer every question with its default")
	accessible := flag.Bool("accessible", false, "ask with plain line-by-line prompts")
	flag.Parse()

	p := ui.New(ui.Options{Defaults: *defaults, Accessible: *accessible})
	if err := run(p); err != nil {
		ui.Stderr().Error("%v", err)
		os.Exit(1)
	}
}

func run(p ui.Prompter) error {
	host, err := p.Menu(ui.Menu{
		Title:   "Restore from which machine?",
		Text:    "Machines in the backup:",
		Default: "desktop",
		Flag:    "--from",
		Options: []ui.Option{
			{Value: "desktop", Note: "newest backup 2026-10-14 03:00"},
			{Value: "laptop", Note: "newest backup 2026-09-30 22:15"},
		},
	})
	if err != nil {
		return err
	}
	fmt.Println("machine:", host)

	sel, err := p.Checklist(ui.Checklist{
		Title: "Restore from " + host + " onto this machine",
		Text: "Space ticks or unticks, → opens a bundle to choose single items, Enter continues. " +
			"Unticked bundles are skipped. Nothing changes until you confirm the plan.",
		Flag:   "--bundle",
		Groups: sampleGroups(),
	})
	if err != nil {
		return err
	}
	fmt.Println("bundles:", strings.Join(sel.Groups, ", "))
	fmt.Println("unticked items:", strings.Join(sel.Unticked, ", "))

	name, err := p.Input(ui.Input{
		Title:   "Machine name",
		Text:    "How this machine is called in the backup.",
		Default: "laptop",
		Flag:    "--name",
		Validate: func(s string) error {
			if strings.TrimSpace(s) == "" || strings.ContainsAny(s, "/ ") {
				return errors.New("use letters, digits and dashes")
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	fmt.Println("name:", name)

	pw, err := p.Password(ui.Password{
		Title: "Backup password",
		Text:  "Nothing shows while you type.",
		Twice: true,
		Flag:  "--password-file",
	})
	if err != nil {
		fmt.Println("password:", err)
	} else {
		fmt.Printf("password: %d characters\n", len(pw))
	}

	ok, err := p.Confirm(ui.Confirm{Title: "Apply this plan?", NoDefault: true, Flag: "--yes"})
	if err != nil {
		return err
	}
	fmt.Println("apply:", ok)
	return nil
}

func sampleGroups() []ui.Group {
	groups := []ui.Group{
		{ID: "base", Title: "Base system", Summary: "112 packages, pacman.conf, locale", Reason: "same distribution", On: true},
		{ID: "app-obs-studio", Title: "OBS Studio", Summary: "1 package, 4 settings files", On: true, Items: []ui.Item{
			{ID: "pkg:obs-studio", Label: "obs-studio (package)", On: true},
			{ID: "/home/alice/.config/obs-studio/global.ini", Label: "~/.config/obs-studio/global.ini", On: true},
			{ID: "/home/alice/.config/obs-studio/basic/profiles", Label: "~/.config/obs-studio/basic/profiles/", On: true},
			{ID: "/home/alice/.config/obs-studio/basic/scenes", Label: "~/.config/obs-studio/basic/scenes/", On: true},
			{ID: "/home/alice/.config/obs-studio/plugin_config", Label: "~/.config/obs-studio/plugin_config/", On: false},
		}},
		{ID: "hw-nvidia", Title: "NVIDIA driver", Summary: "nvidia-dkms, nvidia-utils, modprobe.d/nvidia.conf",
			Reason: "needs hardware this machine doesn't have"},
		{ID: "identity", Title: "Machine identity", Summary: "hostname, machine-id", Reason: "not the same machine"},
		{ID: "bluetooth", Title: "Bluetooth pairings", Summary: "3 devices", Reason: "not the same machine"},
	}
	for i := range 15 {
		groups = append(groups, ui.Group{
			ID: fmt.Sprintf("app-%d", i), Title: fmt.Sprintf("App %d", i+1), Summary: "1 package", On: true,
		})
	}
	return groups
}
