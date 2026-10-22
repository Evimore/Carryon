package cli

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"

	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/agent"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/sysexec"
)

// agentDesktop connects to the notification service in the user's session; tests replace it.
var agentDesktop = func() (agent.Desktop, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to your desktop session (%v): carryon agent runs inside a desktop session, started by carryon-agent.service", err)
	}
	return agent.NewBus(conn)
}

// agentUser is who runs the agent; tests replace it.
var agentUser = currentUser

// agentRunner runs systemctl and pacman for the agent; tests replace it.
var agentRunner = func() sysexec.Runner { return sysexec.New() }

func newAgentCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "agent",
		Short: "Show your desktop notifications about backups",
		Long: `Shows desktop notifications about backups to the user who runs it: a failed backup or
check, a folder Carryon suggests backing up, and saved settings it can restore, each once,
with buttons that start what they say through systemd and polkit.

carryon-agent.service, a user unit, runs it in every desktop session; carryon init switches
it on for everyone. It never runs as root, and writes only in your home folder
(~/.local/state/carryon). Your own settings (~/.config/carryon/config.toml, notify.desktop)
switch it off.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if geteuid() == 0 {
				return errors.New("carryon agent shows one user's notifications and never runs as root. It starts by itself in each desktop session (carryon-agent.service)")
			}
			u, err := agentUser()
			if err != nil {
				return err
			}
			desk, err := agentDesktop()
			if err != nil {
				return err
			}
			log := logger(cmd.ErrOrStderr())
			run := agentRunner()
			a := &agent.Agent{
				Dirs: dirs, User: u, Desktop: desk, Runner: run,
				StateDir: agent.StateDir(u.Home, os.Getenv("XDG_STATE_HOME")),
				Running:  agent.Apps{Runner: run, UID: u.UID}.Running,
				Warn:     log.Warn,
			}
			return a.Run(cmd.Context())
		},
	}
}

// currentUser is the user Carryon runs as.
func currentUser() (sysexec.User, error) {
	u, err := user.Current()
	if err != nil {
		return sysexec.User{}, fmt.Errorf("finding out who you are: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return sysexec.User{}, fmt.Errorf("your user ID %q isn't a number", u.Uid)
	}
	return sysexec.User{Name: u.Username, UID: uid, Home: u.HomeDir}, nil
}
