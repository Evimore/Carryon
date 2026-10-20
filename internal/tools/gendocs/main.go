// Command gendocs writes Carryon's man page and shell completions from the command tree, so they
// always match --help. It runs at build time (make docs) and is not part of the installed binary:
//
//	go run ./internal/tools/gendocs -out build -version 0.1.0
//
// It writes OUT/man/carryon.1 and OUT/completions/{carryon.bash,_carryon,carryon.fish}.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Evimore/carryon/internal/cli"
	"github.com/Evimore/carryon/internal/paths"
)

func main() {
	out := flag.String("out", "build", "folder to write man/ and completions/ into")
	ver := flag.String("version", "dev", "version shown in the man page footer")
	flag.Parse()
	if err := run(*out, *ver); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

func run(out, ver string) error {
	root := newRoot()
	var man bytes.Buffer
	if err := writeMan(&man, root, ver, buildDate()); err != nil {
		return err
	}
	files := map[string][]byte{"man/carryon.1": man.Bytes()}

	gens := map[string]func(io.Writer) error{
		"completions/carryon.bash": func(w io.Writer) error { return root.GenBashCompletionV2(w, true) },
		"completions/_carryon":     root.GenZshCompletion,
		"completions/carryon.fish": func(w io.Writer) error { return root.GenFishCompletion(w, true) },
	}
	for name, gen := range gens {
		var b bytes.Buffer
		if err := gen(&b); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
		files[name] = b.Bytes()
	}
	for name, data := range files {
		p := filepath.Join(out, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", p, err)
		}
	}
	return nil
}

// newRoot builds the command tree as the binary sees it, with cobra's completion command.
func newRoot() *cobra.Command {
	// The standard folders, not paths.Default(): the page describes an installed system, whatever
	// CARRYON_* variables the build happens to run with.
	dirs := paths.Dirs{
		Conf:  "/etc/carryon",
		State: "/var/lib/carryon",
		Cache: "/var/cache/carryon",
		Run:   "/run/carryon",
		Share: "/usr/share/carryon",
	}
	root := cli.NewRoot(io.Discard, dirs)
	root.InitDefaultCompletionCmd()
	root.DisableAutoGenTag = true
	return root
}

// buildDate honours SOURCE_DATE_EPOCH, so package builds are reproducible.
func buildDate() time.Time {
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		var sec int64
		if _, err := fmt.Sscan(s, &sec); err == nil {
			return time.Unix(sec, 0).UTC()
		}
	}
	return time.Now().UTC()
}

// writeMan writes carryon(1) in roff: one page with every command a user can run.
func writeMan(w io.Writer, root *cobra.Command, ver string, date time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, ".TH CARRYON 1 %q %q \"Carryon Manual\"\n", date.Format("2006-01-02"), "carryon "+ver)
	b.WriteString(".SH NAME\n")
	fmt.Fprintf(&b, "carryon \\- %s\n", text(lowerFirst(root.Short)))
	b.WriteString(".SH SYNOPSIS\n")
	b.WriteString(".B carryon\n.I command\n.RI [ options ]\n")
	b.WriteString(".SH DESCRIPTION\n")
	paragraphs(&b, root.Long)

	b.WriteString(".SH COMMANDS\n")
	for _, c := range visible(root) {
		writeCommand(&b, c)
	}

	if fs := root.PersistentFlags(); fs.HasAvailableFlags() {
		b.WriteString(".SH GLOBAL OPTIONS\n")
		writeFlags(&b, fs)
	}

	b.WriteString(`.SH FILES
.TP
.I /etc/carryon/
Settings: config.toml, excludes.txt, bundles.conf and the repository password. Written by
.BR "carryon init" .
.TP
.I /var/lib/carryon/
The system record, run history, status summary and flags.
.TP
.I /var/cache/carryon/restic/
restic's cache.
.TP
.I /usr/share/carryon/
Default excludes and the app catalog, shipped with the package.
.TP
.I /run/motd.d/carryon.motd
One line shown at console and SSH logins while something needs attention. Turn it off with
.BR "notify.login_message = false" .
.TP
.I ~/.config/carryon/config.toml
A user's own excludes and notification settings for their home folder.
.SH ENVIRONMENT
.TP
.BR CARRYON_CONF_DIR ", " CARRYON_STATE_DIR ", " CARRYON_CACHE_DIR ", " CARRYON_RUN_DIR ", " CARRYON_SHARE_DIR ", " CARRYON_MOTD_DIR
Use another folder instead of the standard one, for testing.
.SH EXIT STATUS
0 on success, 1 on failure.
.B carryon status --json
exits with 2 when something needs attention.
.SH SEE ALSO
.BR restic (1),
.BR pacman (8),
.BR systemd.timer (5)
.PP
https://github.com/Evimore/carryon
`)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeCommand(b *strings.Builder, c *cobra.Command) {
	if c.Name() == "completion" {
		// cobra's own help for it talks about other systems; the package installs completions.
		var shells []string
		for _, sub := range visible(c) {
			shells = append(shells, sub.Name())
		}
		fmt.Fprintf(b, ".SS carryon completion %s\n", text(strings.Join(shells, "|")))
		b.WriteString(".PP\nPrint the shell completion script. The package installs them for bash, zsh and fish already.\n")
		return
	}
	fmt.Fprintf(b, ".SS %s\n", text(c.UseLine()))
	if c.Long != "" {
		paragraphs(b, c.Long)
	} else {
		paragraphs(b, c.Short)
	}
	if len(c.Aliases) > 0 {
		fmt.Fprintf(b, ".PP\nAlso: %s.\n", text(strings.Join(c.Aliases, ", ")))
	}
	if c.Example != "" {
		b.WriteString(".PP\nExamples:\n.PP\n.nf\n.RS 4\n")
		for _, l := range strings.Split(strings.TrimRight(c.Example, "\n"), "\n") {
			b.WriteString(text(strings.TrimSpace(l)) + "\n")
		}
		b.WriteString(".RE\n.fi\n")
	}
	if fs := c.NonInheritedFlags(); fs.HasAvailableFlags() {
		b.WriteString(".PP\nOptions:\n.RS 4\n")
		writeFlags(b, fs)
		b.WriteString(".RE\n")
	}
	for _, sub := range visible(c) {
		writeCommand(b, sub)
	}
}

func writeFlags(b *strings.Builder, fs *pflag.FlagSet) {
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		name := "--" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", " + name
		}
		arg, usage := pflag.UnquoteUsage(f)
		head := "\\fB" + text(name) + "\\fR"
		if arg != "" {
			head += " \\fI" + text(arg) + "\\fR"
		}
		fmt.Fprintf(b, ".TP\n%s\n%s", head, text(usage))
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "[]" && f.DefValue != "0" {
			fmt.Fprintf(b, " (default: %s)", text(f.DefValue))
		}
		b.WriteString("\n")
	})
}

// visible lists the subcommands shown in --help.
func visible(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range c.Commands() {
		if sub.IsAvailableCommand() {
			out = append(out, sub)
		}
	}
	return out
}

// paragraphs turns help text into roff paragraphs, keeping indented lines as they are.
func paragraphs(b *strings.Builder, s string) {
	for _, para := range strings.Split(strings.TrimSpace(s), "\n\n") {
		lines := strings.Split(para, "\n")
		indented := false
		for _, l := range lines {
			if strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "\t") {
				indented = true
			}
		}
		b.WriteString(".PP\n")
		if indented {
			b.WriteString(".nf\n")
		}
		for _, l := range lines {
			if !indented {
				l = strings.TrimSpace(l)
			}
			b.WriteString(text(l) + "\n")
		}
		if indented {
			b.WriteString(".fi\n")
		}
	}
}

// text escapes a line for roff: backslashes, hyphens and a leading dot or quote.
func text(s string) string {
	s = strings.ReplaceAll(s, `\`, `\e`)
	s = strings.ReplaceAll(s, "-", `\-`)
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "'") {
		s = `\&` + s
	}
	return s
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
