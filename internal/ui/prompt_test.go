package ui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func notEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("type a name")
	}
	return nil
}

func TestDefaults(t *testing.T) {
	var d Defaults
	sel, err := d.Checklist(sampleChecklist())
	if err != nil || !reflect.DeepEqual(sel, Selection{Groups: []string{"base", "app-obs-studio"}}) {
		t.Errorf("Checklist = %+v, %v", sel, err)
	}

	menu := Menu{Title: "Restore from which machine?", Flag: "--from",
		Options: []Option{{Value: "desktop"}, {Value: "laptop"}}}
	if _, err := d.Menu(menu); !isNeedsAnswer(err, "--from") {
		t.Errorf("Menu without a default: %v", err)
	}
	menu.Default = "laptop"
	if v, err := d.Menu(menu); v != "laptop" || err != nil {
		t.Errorf("Menu = %q, %v", v, err)
	}
	menu.Default = "server"
	if _, err := d.Menu(menu); !isNeedsAnswer(err, "--from") {
		t.Errorf("Menu with a default that isn't an option: %v", err)
	}

	if v, err := d.Confirm(Confirm{Title: "Run a backup now?", Default: true}); !v || err != nil {
		t.Errorf("Confirm = %v, %v", v, err)
	}
	if _, err := d.Confirm(Confirm{Title: "Apply this plan?", NoDefault: true, Flag: "--yes"}); !isNeedsAnswer(err, "--yes") {
		t.Errorf("Confirm without a safe default: %v", err)
	}

	if v, err := d.Input(Input{Title: "Machine name", Default: "desktop", Validate: notEmpty}); v != "desktop" || err != nil {
		t.Errorf("Input = %q, %v", v, err)
	}
	if _, err := d.Input(Input{Title: "Repository", Flag: "--repository"}); !isNeedsAnswer(err, "--repository") {
		t.Errorf("Input without a default: %v", err)
	}
	if _, err := d.Input(Input{Title: "Machine name", Default: " ", Validate: notEmpty, Flag: "--name"}); !isNeedsAnswer(err, "--name") {
		t.Errorf("Input with an invalid default: %v", err)
	}

	if _, err := d.Password(Password{Title: "Backup password", Flag: "--password-file"}); !isNeedsAnswer(err, "--password-file") {
		t.Errorf("Password: %v", err)
	}
}

func isNeedsAnswer(err error, flag string) bool {
	var n *NeedsAnswerError
	return errors.As(err, &n) && n.Flag == flag
}

func TestNeedsAnswerMessage(t *testing.T) {
	cases := []struct {
		err  NeedsAnswerError
		want string
	}{
		{NeedsAnswerError{"Apply this plan?", "--yes"},
			`Carryon can't answer "Apply this plan?" on its own: run it in a terminal, or use --yes`},
		{NeedsAnswerError{"Backup password", ""},
			`Carryon can't answer "Backup password" on its own: run it in a terminal to answer it`},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
}

func TestNewWithoutATerminalTakesDefaults(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, ok := New(Options{In: f, Out: f}).(Defaults); !ok {
		t.Error("New with a file for stdin didn't return Defaults")
	}
	if _, ok := New(Options{Defaults: true, In: f, Out: f}).(Defaults); !ok {
		t.Error("New with Defaults set didn't return Defaults")
	}
}

func TestSelectionHas(t *testing.T) {
	s := Selection{Groups: []string{"app-obs-studio"}, Unticked: []string{"pkg:obs-studio"}}
	cases := []struct {
		group, item string
		want        bool
	}{
		{"app-obs-studio", "/home/alice/.config/obs-studio/basic.ini", true},
		{"app-obs-studio", "pkg:obs-studio", false},
		{"base", "pkg:linux", false},
	}
	for _, tc := range cases {
		if got := s.HasItem(tc.group, tc.item); got != tc.want {
			t.Errorf("HasItem(%q, %q) = %v", tc.group, tc.item, got)
		}
	}
}

func TestScript(t *testing.T) {
	s := &Script{Answers: []any{
		[]string{"hw-nvidia"},
		"laptop",
		true,
		UseDefault,
		"correct horse",
		ErrCancelled,
	}}
	sel, err := s.Checklist(sampleChecklist())
	if err != nil || !reflect.DeepEqual(sel, Selection{Groups: []string{"hw-nvidia"}}) {
		t.Errorf("Checklist = %+v, %v", sel, err)
	}
	if v, err := s.Menu(Menu{Title: "Restore from which machine?", Options: []Option{{Value: "desktop"}, {Value: "laptop"}}}); v != "laptop" || err != nil {
		t.Errorf("Menu = %q, %v", v, err)
	}
	if v, err := s.Confirm(Confirm{Title: "Apply this plan?", NoDefault: true}); !v || err != nil {
		t.Errorf("Confirm = %v, %v", v, err)
	}
	if v, err := s.Input(Input{Title: "Machine name", Default: "desktop"}); v != "desktop" || err != nil {
		t.Errorf("Input = %q, %v", v, err)
	}
	if v, err := s.Password(Password{Title: "Backup password", Twice: true}); v != "correct horse" || err != nil {
		t.Errorf("Password = %q, %v", v, err)
	}
	if _, err := s.Confirm(Confirm{Title: "Run a backup now?"}); !errors.Is(err, ErrCancelled) {
		t.Errorf("an error answer: %v", err)
	}
	if _, err := s.Confirm(Confirm{Title: "One more?"}); err == nil || !strings.Contains(err.Error(), `no answer left for the confirm "One more?"`) {
		t.Errorf("out of answers: %v", err)
	}
	want := []Asked{
		{"checklist", "Restore from desktop onto laptop"}, {"menu", "Restore from which machine?"},
		{"confirm", "Apply this plan?"}, {"input", "Machine name"}, {"password", "Backup password"},
		{"confirm", "Run a backup now?"}, {"confirm", "One more?"},
	}
	if !reflect.DeepEqual(s.Asked, want) {
		t.Errorf("Asked = %+v", s.Asked)
	}
}

func TestScriptRejectsAnswersThatDontFit(t *testing.T) {
	cases := []struct {
		name string
		ask  func(*Script) error
		ans  any
	}{
		{"bool for a menu", func(s *Script) error {
			_, err := s.Menu(Menu{Title: "Pick", Options: []Option{{Value: "a"}}})
			return err
		}, true},
		{"unknown option", func(s *Script) error {
			_, err := s.Menu(Menu{Title: "Pick", Options: []Option{{Value: "a"}}})
			return err
		}, "b"},
		{"unknown group", func(s *Script) error {
			_, err := s.Checklist(sampleChecklist())
			return err
		}, []string{"nope"}},
		{"invalid input", func(s *Script) error {
			_, err := s.Input(Input{Title: "Machine name", Validate: notEmpty})
			return err
		}, ""},
		{"no default", func(s *Script) error {
			_, err := s.Password(Password{Title: "Backup password", Flag: "--password-file"})
			return err
		}, UseDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.ask(&Script{Answers: []any{tc.ans}}); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

// Other packages test code that asks questions by handing it a Script with the answers.
func ExampleScript() {
	s := &Script{Answers: []any{
		[]string{"base"}, // tick only the "base" bundle
		true,             // yes, apply the plan
	}}
	sel, _ := s.Checklist(Checklist{Title: "Restore from desktop onto laptop", Groups: []Group{
		{ID: "base", Title: "Base system", On: true},
		{ID: "hw-nvidia", Title: "NVIDIA driver", Reason: "needs hardware this machine doesn't have"},
	}})
	ok, _ := s.Confirm(Confirm{Title: "Apply this plan?", NoDefault: true, Flag: "--yes"})
	fmt.Println(sel.Groups, ok)
	for _, q := range s.Asked {
		fmt.Printf("%s: %s\n", q.Kind, q.Title)
	}
	// Output:
	// [base] true
	// checklist: Restore from desktop onto laptop
	// confirm: Apply this plan?
}

// The accessible checklist reads numbers and commands line by line.
func TestPlainChecklist(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  Selection
		err   error
	}{
		{"enter keeps the defaults", "\n", Selection{Groups: []string{"base", "app-obs-studio"}}, nil},
		{"toggle by number", "1 3\n\n", Selection{Groups: []string{"app-obs-studio", "hw-nvidia"}}, nil},
		{"all and none", "none\n4\n\n", Selection{Groups: []string{"identity"}}, nil},
		{"open a group", "open 2\n1\n\n\n", Selection{Groups: []string{"base", "app-obs-studio"}, Unticked: []string{"pkg:obs-studio"}}, nil},
		{"q inside a group drops its changes", "open 2\nnone\nq\n\n", Selection{Groups: []string{"base", "app-obs-studio"}}, nil},
		{"bad open is ignored", "open 1\nopen 9\n\n", Selection{Groups: []string{"base", "app-obs-studio"}}, nil},
		{"end of input after a command cancels", "1", Selection{}, ErrCancelled},
		{"q cancels", "q\n", Selection{}, ErrCancelled},
		{"end of input cancels", "", Selection{}, ErrCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			term := NewTerminal(strings.NewReader(tc.input), &out, true)
			got, err := term.Checklist(sampleChecklist())
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v\n%s", err, tc.err, out.String())
			}
			if err == nil && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v\nwant %+v\n%s", got, tc.want, out.String())
			}
		})
	}
}

func TestPlainChecklistShowsReasons(t *testing.T) {
	var out bytes.Buffer
	if _, err := NewTerminal(strings.NewReader("\n"), &out, true).Checklist(sampleChecklist()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Restore from desktop onto laptop",
		"  1. [x] Base system",
		"12 packages · same distribution",
		"  2. [x] OBS Studio (3 of 3 items)",
		"  3. [ ] NVIDIA driver",
		"3 packages · needs hardware this machine doesn't have",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// The accessible menus, confirmations and inputs are huh's; check they're wired up right.
func TestAccessiblePrompts(t *testing.T) {
	ask := func(input string) *Terminal {
		return NewTerminal(strings.NewReader(input), &bytes.Buffer{}, true)
	}
	menu := Menu{Title: "Restore from which machine?", Default: "laptop",
		Options: []Option{{Value: "desktop", Note: "newest backup 2026-10-14"}, {Value: "laptop"}}}
	if v, err := ask("1\n").Menu(menu); v != "desktop" || err != nil {
		t.Errorf("Menu = %q, %v", v, err)
	}
	if v, err := ask("\n").Menu(menu); v != "laptop" || err != nil {
		t.Errorf("Menu with Enter = %q, %v", v, err)
	}
	if v, err := ask("y\n").Confirm(Confirm{Title: "Apply this plan?"}); !v || err != nil {
		t.Errorf("Confirm = %v, %v", v, err)
	}
	if v, err := ask("\n").Confirm(Confirm{Title: "Run a backup now?", Default: true}); !v || err != nil {
		t.Errorf("Confirm with Enter = %v, %v", v, err)
	}
	if v, err := ask("\nlaptop\n").Input(Input{Title: "Machine name", Validate: notEmpty}); v != "laptop" || err != nil {
		t.Errorf("Input = %q, %v", v, err)
	}
	if v, err := ask("\n").Input(Input{Title: "Machine name", Default: "desktop", Validate: notEmpty}); v != "desktop" || err != nil {
		t.Errorf("Input with Enter = %q, %v", v, err)
	}
	if _, err := ask("").Input(Input{Title: "Machine name", Validate: notEmpty}); !errors.Is(err, ErrCancelled) {
		t.Errorf("Input at the end of input: %v", err)
	}
}

func TestAccessiblePromptsShowTheText(t *testing.T) {
	var out bytes.Buffer
	c := Confirm{Title: "Run a backup now?", Text: "It takes a few minutes the first time."}
	if _, err := NewTerminal(strings.NewReader("n\n"), &out, true).Confirm(c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), c.Text) {
		t.Errorf("the text isn't shown:\n%s", out.String())
	}
}
