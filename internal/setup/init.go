package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/ui"
)

// Options are init's answers given as flags. Empty ones are asked, or taken from the existing
// settings.
type Options struct {
	Repo            string   // --repo
	PasswordFile    string   // --password-file: read the password from this file
	PasswordCommand string   // --password-command: a command that prints the password
	MachineName     string   // --machine-name
	Users           []string // --users
	Schedule        string   // --schedule
	VerifySchedule  string   // --verify-schedule
	RcloneUser      string   // --rclone-user
	SettingsFrom    string   // --settings-from (--connect only): a machine name, or "none"
	Yes             bool     // --yes: accept the destination's risks without asking
	KitSaved        bool     // --kit-saved: the recovery kit is saved, don't ask
}

// Init sets up backups on this machine: destination, password, machine name, users and schedules.
// It creates the repository, or opens an existing one, records the destination, writes the
// settings, switches on the timers and shows the recovery kit.
func (e *Env) Init(ctx context.Context, o Options) error {
	if err := e.checkRestic(ctx); err != nil {
		return err
	}
	cfg, err := e.loadExisting()
	if err != nil {
		return err
	}

	url := o.Repo
	if url == "" {
		url, err = e.Prompt.Input(ui.Input{
			Title:       "Where should backups go?",
			Text:        "A folder on another drive, or any restic repository URL (sftp:, rest:, s3:, b2:, rclone: and others).",
			Placeholder: "/mnt/backup/carryon",
			Default:     cfg.Repository.URL,
			Validate:    func(s string) error { _, err := normalizeURL(s); return err },
			Flag:        "--repo",
		})
		if err != nil {
			return err
		}
	}
	if url, err = normalizeURL(url); err != nil {
		return err
	}
	if url, err = e.guardDestination(url, o.Yes); err != nil {
		return err
	}
	rcloneUser, err := e.chooseRcloneUser(url, o.RcloneUser, cfg.Repository.RcloneUser)
	if err != nil {
		return err
	}

	sec, state, repoID, err := e.initPassword(ctx, url, rcloneUser, o, cfg.Repository)
	if err != nil {
		return err
	}
	defer sec.discard()
	eng := e.engine(withRclone(sec.repo(url), rcloneUser), "")

	var list []Machine
	if state == repoOpens {
		if list, err = listMachines(ctx, eng); err != nil {
			return err
		}
	}
	name, err := e.chooseMachineName(ctx, eng, list, o.MachineName, orDefault(cfg.MachineName, e.defaultMachineName()))
	if err != nil {
		return err
	}
	if err := e.chooseUsersAndSchedules(&cfg, o); err != nil {
		return err
	}
	if state == repoOpens {
		if err := e.checkSameRepository(url, repoID, cfg.Repository.URL, o.Yes); err != nil {
			return err
		}
	}

	// The password is saved before the repository is created, so it can't be lost in between.
	repo, err := e.commit(sec, url)
	if err != nil {
		return err
	}
	repo.RcloneUser = rcloneUser
	eng = e.engine(repo, name)
	if state == repoMissing {
		e.Log.Info("Creating the encrypted repository at %s", url)
		r, err := eng.Run(ctx, "init")
		if err != nil {
			return err
		}
		if err := r.Err(); err != nil {
			return fmt.Errorf("creating the repository at %s: %w", url, err)
		}
	} else {
		e.Log.Info("The repository at %s exists and the password opens it.", url)
	}

	cfg.MachineName = name
	cfg.Repository = repo
	if err := e.finish(ctx, eng, cfg); err != nil {
		return err
	}
	if err := e.showKit(Kit{URL: url, MachineName: name, Password: sec.password, PasswordCommand: repo.PasswordCommand, RcloneUser: rcloneUser}, o.KitSaved); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "Backups are set up: they run %s, and after package changes.\n", cfg.Backup.Schedule)
	fmt.Fprintln(e.Out, `See how they're doing with "carryon status".`)
	return nil
}

// finish records the destination, saves the settings and switches on the timers: the steps every
// kind of init ends with.
func (e *Env) finish(ctx context.Context, eng *engine.Engine, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("the settings would have problems, so nothing more was changed:\n%w", err)
	}
	id, err := e.recordDestination(ctx, cfg.Repository.URL, eng)
	if err != nil {
		return err
	}
	if err := writeIdentity(e.Dirs, id); err != nil {
		return err
	}
	if err := os.MkdirAll(e.Dirs.Conf, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", e.Dirs.Conf, err)
	}
	if err := config.Write(e.Dirs.ConfigFile(), cfg); err != nil {
		return err
	}
	e.Log.Info("Saved the settings in %s", e.Dirs.ConfigFile())
	e.installUnits(ctx, cfg)
	return nil
}

// checkRestic fails plainly when restic is missing or too old.
func (e *Env) checkRestic(ctx context.Context) error {
	_, err := e.engine(config.Repository{}, "").CheckVersion(ctx)
	return err
}

// loadExisting reads the current settings, so running init again keeps them, or returns the
// defaults when there are none.
func (e *Env) loadExisting() (config.Config, error) {
	file := e.Dirs.ConfigFile()
	cfg, warnings, err := config.Load(file)
	if errors.Is(err, fs.ErrNotExist) {
		return config.Default(), nil
	}
	if err != nil {
		return config.Config{}, fmt.Errorf("%w\nFix the file or move it away, then run init again", err)
	}
	for _, w := range warnings {
		e.Log.Warn("%s", w)
	}
	return cfg, nil
}

func withRclone(r config.Repository, user string) config.Repository {
	r.RcloneUser = user
	return r
}

// chooseRcloneUser asks whose rclone login reaches an rclone: repository.
func (e *Env) chooseRcloneUser(url, given, existing string) (string, error) {
	if !strings.HasPrefix(url, "rclone:") {
		return "", nil
	}
	if given != "" {
		return given, nil
	}
	def := existing
	if def == "" {
		if u := os.Getenv("SUDO_USER"); u != "root" {
			def = u
		}
	}
	return e.Prompt.Input(ui.Input{
		Title:   "Whose rclone login should Carryon use?",
		Text:    "Backups run as root, with the rclone remote this user set up with \"rclone config\".",
		Default: def,
		Flag:    "--rclone-user",
	})
}

// initPassword finds the password for init: from the flags, from the existing settings, or by
// asking. It returns the password staged, whether a repository exists at url, and its ID.
func (e *Env) initPassword(ctx context.Context, url, rcloneUser string, o Options, old config.Repository) (*secret, repoState, string, error) {
	try := func(s *secret) (repoState, string, error) {
		return probe(ctx, e.engine(withRclone(s.repo(url), rcloneUser), ""))
	}

	var sec *secret
	fromFlag := true
	switch {
	case o.PasswordCommand != "":
		sec = &secret{command: o.PasswordCommand}
	case o.PasswordFile != "":
		pw, err := ReadPasswordFile(o.PasswordFile)
		if err != nil {
			return nil, 0, "", err
		}
		if sec, err = e.newSecret(pw); err != nil {
			return nil, 0, "", err
		}
	case old.PasswordCommand != "":
		sec, fromFlag = &secret{command: old.PasswordCommand}, false
	default:
		fromFlag = false
		if pw, err := ReadPasswordFile(orDefault(old.PasswordFile, e.Dirs.PasswordFile())); err == nil {
			if sec, err = e.newSecret(pw); err != nil {
				return nil, 0, "", err
			}
		}
	}
	if sec != nil {
		state, id, err := try(sec)
		if err != nil {
			sec.discard()
			return nil, 0, "", err
		}
		if state != repoWrongKey {
			return sec, state, id, nil
		}
		sec.discard()
		if fromFlag {
			return nil, 0, "", fmt.Errorf("there is already a backup repository at %s, and that password doesn't open it", url)
		}
		e.Log.Warn("The saved password doesn't open the repository at %s.", url)
	}

	// Find out whether a repository is there with a password that can't open it.
	throwaway, err := GeneratePassword(e.rand())
	if err != nil {
		return nil, 0, "", err
	}
	probeSec, err := e.newSecret(throwaway)
	if err != nil {
		return nil, 0, "", err
	}
	state, _, err := try(probeSec)
	probeSec.discard()
	if err != nil {
		return nil, 0, "", err
	}
	if state != repoMissing {
		pw, err := e.askExistingPassword(url, try)
		if err != nil {
			return nil, 0, "", err
		}
		return e.openWith(pw, try)
	}
	sec, err = e.newPassword()
	if err != nil {
		return nil, 0, "", err
	}
	return sec, repoMissing, "", nil
}

// askExistingPassword asks for the password of the repository at url until one opens it.
func (e *Env) askExistingPassword(url string, try func(*secret) (repoState, string, error)) (string, error) {
	return e.Prompt.Password(ui.Password{
		Title: "Repository password",
		Text:  "There is already a backup repository at " + url + ". Enter its password (it's in your recovery kit).",
		Validate: func(pw string) error {
			s, err := e.newSecret(pw)
			if err != nil {
				return err
			}
			defer s.discard()
			state, _, err := try(s)
			if err != nil {
				return err
			}
			if state != repoOpens {
				return errors.New("that password doesn't open the repository")
			}
			return nil
		},
		Flag: "--password-file",
	})
}

// openWith stages pw and opens the repository with it.
func (e *Env) openWith(pw string, try func(*secret) (repoState, string, error)) (*secret, repoState, string, error) {
	sec, err := e.newSecret(pw)
	if err != nil {
		return nil, 0, "", err
	}
	state, id, err := try(sec)
	if err == nil && state != repoOpens {
		err = errors.New("that password doesn't open the repository")
	}
	if err != nil {
		sec.discard()
		return nil, 0, "", err
	}
	return sec, state, id, nil
}

// newPassword asks how to protect a new repository: a generated password (the default), one the
// user types, or a command that prints one.
func (e *Env) newPassword() (*secret, error) {
	how, err := e.Prompt.Menu(ui.Menu{
		Title: "Password for the new repository",
		Text:  "The backup is encrypted with this password. Without it nobody can open the backup, including you.",
		Options: []ui.Option{
			{Value: "generate", Label: "Generate a strong password", Note: "recommended"},
			{Value: "enter", Label: "Type a password of my own"},
			{Value: "command", Label: "Get it from a command", Note: "such as systemd-creds"},
		},
		Default: "generate",
		Flag:    "--password-file",
	})
	if err != nil {
		return nil, err
	}
	switch how {
	case "enter":
		pw, err := e.Prompt.Password(ui.Password{
			Title: "New repository password",
			Twice: true,
			Validate: func(pw string) error {
				if err := checkPassword(pw); err != nil {
					return err
				}
				if len(pw) < 12 {
					return errors.New("use at least 12 characters")
				}
				return nil
			},
			Flag: "--password-file",
		})
		if err != nil {
			return nil, err
		}
		return e.newSecret(pw)
	case "command":
		cmd, err := e.Prompt.Input(ui.Input{
			Title:       "Password command",
			Text:        "A command that prints the password, run every time Carryon opens the repository.",
			Placeholder: "systemd-creds decrypt /etc/carryon/repository-password.cred",
			Validate: func(s string) error {
				if strings.TrimSpace(s) == "" || strings.ContainsAny(s, "\n\r") {
					return errors.New("enter the command on one line")
				}
				return nil
			},
			Flag: "--password-command",
		})
		if err != nil {
			return nil, err
		}
		return &secret{command: strings.TrimSpace(cmd)}, nil
	}
	pw, err := GeneratePassword(e.rand())
	if err != nil {
		return nil, err
	}
	return e.newSecret(pw)
}

// chooseUsersAndSchedules asks whose homes to back up and when.
func (e *Env) chooseUsersAndSchedules(cfg *config.Config, o Options) error {
	users := o.Users
	if len(users) == 0 {
		s, err := e.Prompt.Input(ui.Input{
			Title:   "Whose home folders should be backed up?",
			Text:    "\"*\" means every user who can sign in. Or list names, separated by commas.",
			Default: strings.Join(cfg.Backup.Users, ", "),
			Validate: func(s string) error {
				if len(splitList(s)) == 0 {
					return errors.New("enter * or at least one user name")
				}
				return nil
			},
			Flag: "--users",
		})
		if err != nil {
			return err
		}
		users = splitList(s)
	}
	cfg.Backup.Users = users

	for _, s := range []struct {
		dst          *string
		given, title string
		text, flag   string
	}{
		{&cfg.Backup.Schedule, o.Schedule, "When should the regular backup run?", "A systemd calendar time: \"daily\", \"hourly\", \"Mon..Fri 12:00\".", "--schedule"},
		{&cfg.Verify.Schedule, o.VerifySchedule, "When should the deep check run?", "It downloads a little of the backup to prove it can be restored. A systemd calendar time, such as \"weekly\".", "--verify-schedule"},
	} {
		v := s.given
		if v == "" {
			var err error
			v, err = e.Prompt.Input(ui.Input{Title: s.title, Text: s.text, Default: *s.dst, Validate: checkSchedule, Flag: s.flag})
			if err != nil {
				return err
			}
		}
		if err := checkSchedule(v); err != nil {
			return fmt.Errorf("%s %q: %w", s.flag, v, err)
		}
		*s.dst = strings.TrimSpace(v)
	}
	return nil
}

// checkSameRepository makes the user confirm before recording a different repository at the
// place backups went before: a different disk at the same mount point looks just like this.
func (e *Env) checkSameRepository(url, id, oldURL string, yes bool) error {
	prev, ok, err := ReadIdentity(e.Dirs)
	if err != nil || !ok || prev.RepoID == "" || prev.RepoID == id || oldURL != url {
		return err
	}
	accept, err := e.confirmRisk(yes, ui.Confirm{
		Title: "Use a different repository?",
		Text: fmt.Sprintf("The repository at %s isn't the one this machine backed up to before (its ID is %s, before it was %s). "+
			"A different drive may be mounted there.", url, id, prev.RepoID),
	})
	if err != nil {
		return err
	}
	if !accept {
		return errors.New("nothing was changed: check which drive is mounted, then run init again")
	}
	return nil
}
