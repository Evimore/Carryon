package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/notify"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// notifyTimeout bounds the whole of carryon notify, so it never hangs the unit that runs it.
const notifyTimeout = 3 * time.Minute

// notifySystem returns what carryon notify uses from the machine; tests replace it.
var notifySystem = func() notify.System {
	r := sysexec.New()
	r.Timeout = 30 * time.Second
	return notify.System{Runner: r, Euid: geteuid(), Path: os.Getenv("PATH")}
}

// notifyErrors returns where carryon notify reads a unit's errors: Carryon's own run history,
// then the journal. Tests replace it.
var notifyErrors = func(dirs paths.Dirs, r sysexec.Runner) notify.ErrorSource {
	return notify.RunHistory{Dirs: dirs, Runner: r, Fallback: notify.Journal{Runner: r}}
}

func newNotifyCmd(dirs paths.Dirs) *cobra.Command {
	var test bool
	cmd := &cobra.Command{
		Use:   "notify UNIT",
		Short: "Tell the user a run failed",
		Long: `Tells the user that a systemd unit failed, through every channel in the [notify] settings:
a desktop notification for each signed-in user whose home is backed up, ntfy and a webhook.
carryon-notify@.service runs it for OnFailure=. A user whose agent runs (carryon-agent.service)
gets the agent's notification instead, with buttons, when the failure is in the run history.

With --test, sends a test message through every channel and reports how each went, so you
can check your setup: sudo carryon notify --test`,
		Hidden: true,
		Args: func(_ *cobra.Command, args []string) error {
			switch {
			case test && len(args) > 0:
				return errors.New("carryon notify --test takes no unit")
			case !test && len(args) != 1:
				return errors.New("carryon notify needs the name of the unit that failed, or --test")
			case !test && strings.HasPrefix(args[0], "-"):
				return fmt.Errorf("%q isn't a unit name", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			log := logger(cmd.ErrOrStderr())
			cfg := notifyConfig(dirs, log)
			sys := notifySystem()
			sys.Warn = log.Warn
			sys.Secrets = append(sys.Secrets, repositorySecrets(cfg.Repository, dirs)...)
			ctx, cancel := context.WithTimeout(cmd.Context(), notifyTimeout)
			defer cancel()
			// A failure in the run history is in status.json too, where each user's agent finds it
			// and shows it with buttons; those users don't need root's plain notification as well.
			if !test && (notify.RunHistory{Dirs: dirs, Runner: sys.Runner}).Recorded(ctx, args[0]) {
				sys.AgentRuns = notify.AgentActive(sys.Runner, sys.Euid, sys.Path)
			}
			senders := notify.Channels(cfg, sys)
			if len(senders) == 0 {
				return notify.ErrNoChannel
			}

			host := machineName(cfg)
			var msg notify.Message
			if test {
				msg = notify.Test(host)
			} else {
				remote := &notify.RemoteCheck{Runner: sys.Runner, Euid: sys.Euid, Path: sys.Path, LookupUser: sys.LookupUser}
				msg = notify.Failure(ctx, args[0], host, cfg.Repository, notifyErrors(dirs, sys.Runner), remote, log.Warn)
			}

			results := notify.SendAll(ctx, msg, senders)
			failed := false
			for _, r := range results {
				switch {
				case r.Err != nil && test:
					fmt.Fprintf(cmd.OutOrStdout(), "failed  %s: %v\n", r.Channel, r.Err)
				case r.Err != nil:
					log.Error("couldn't notify through %s: %v", r.Channel, r.Err)
				case test:
					fmt.Fprintf(cmd.OutOrStdout(), "sent    %s: %s\n", r.Channel, r.To)
				default:
					log.Info("notified through %s: %s", r.Channel, r.To)
				}
				failed = failed || r.Err != nil
			}
			// A failure alert counts as delivered when any channel took it; a test reports every
			// channel that doesn't work, so the user can fix it.
			if !notify.Delivered(results) || (test && failed) {
				return exitError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&test, "test", false, "send a test message through every channel and report each result")
	return cmd
}

// logger writes messages to w, marked for the journal when w is the process's stderr.
func logger(w io.Writer) *ui.Logger {
	if w == os.Stderr {
		return ui.Stderr()
	}
	return ui.NewLogger(w, false, false)
}

// notifyConfig reads the config for carryon notify. A failed run may come from a broken config,
// so problems are warnings, and a missing or unreadable config falls back to the defaults:
// the user still hears that something failed.
func notifyConfig(dirs paths.Dirs, log *ui.Logger) config.Config {
	file := dirs.ConfigFile()
	cfg, warnings, err := config.Load(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		log.Warn("%s doesn't exist: notifying with the default settings", file)
		return config.Default()
	case err != nil:
		log.Warn("%v: notifying with the default settings", err)
		return config.Default()
	}
	for _, w := range warnings {
		log.Warn("%s", w)
	}
	if err := cfg.Validate(); err != nil {
		log.Warn("%s has problems:\n%v", file, err)
	}
	return cfg
}

// machineName is the name messages say they come from.
func machineName(cfg config.Config) string {
	if cfg.MachineName != "" {
		return cfg.MachineName
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

// repositorySecrets returns the repository password, when this process can read it, so it is
// hidden from every message even if an error line quotes it.
func repositorySecrets(repo config.Repository, dirs paths.Dirs) []string {
	if repo.PasswordCommand != "" {
		return nil
	}
	file := repo.PasswordFile
	if file == "" {
		file = dirs.PasswordFile()
	}
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<10))
	if err != nil {
		return nil
	}
	// restic uses the file's content without surrounding blank space; hide its first line too, in
	// case an error quotes only that.
	var secrets []string
	if whole := strings.TrimSpace(string(data)); whole != "" {
		secrets = append(secrets, whole)
	}
	if line, _, ok := strings.Cut(strings.TrimSpace(string(data)), "\n"); ok && strings.TrimSpace(line) != "" {
		secrets = append(secrets, strings.TrimSpace(line))
	}
	return secrets
}
