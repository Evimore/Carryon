package ui

import (
	"fmt"
	"slices"
)

// Script answers questions from a list, for tests of packages that ask the user things. It
// records every question in Asked.
//
// Each answer must fit its question:
//
//   - Checklist: a Selection, or a []string of group IDs to tick (items keep their starting
//     ticks);
//   - Menu: the Value of an option;
//   - Confirm: a bool;
//   - Input and Password: a string, which must pass the question's Validate.
//
// Any question also takes UseDefault (its default, with the same rules as Defaults) or an error,
// which is returned as is (for example ErrCancelled). Running out of answers, or an answer of the
// wrong kind, is an error that names the question.
type Script struct {
	Answers []any
	Asked   []Asked
}

// Asked records one question a Script answered.
type Asked struct {
	Kind  string // "checklist", "menu", "confirm", "input" or "password"
	Title string
}

// UseDefault answers a scripted question with its default.
var UseDefault = useDefault{}

type useDefault struct{}

func (s *Script) next(kind, title string) (any, error) {
	s.Asked = append(s.Asked, Asked{Kind: kind, Title: title})
	if len(s.Answers) == 0 {
		return nil, fmt.Errorf("script: no answer left for the %s %q", kind, title)
	}
	a := s.Answers[0]
	s.Answers = s.Answers[1:]
	if err, ok := a.(error); ok {
		return nil, err
	}
	return a, nil
}

func wrongAnswer(a any, kind, title string) error {
	return fmt.Errorf("script: answer %#v doesn't fit the %s %q", a, kind, title)
}

// Checklist answers with the next Selection or list of group IDs.
func (s *Script) Checklist(c Checklist) (Selection, error) {
	a, err := s.next("checklist", c.Title)
	if err != nil {
		return Selection{}, err
	}
	switch a := a.(type) {
	case useDefault:
		return Defaults{}.Checklist(c)
	case Selection:
		return a, nil
	case []string:
		groups := slices.Clone(c.Groups)
		for i := range groups {
			groups[i].On = slices.Contains(a, groups[i].ID)
		}
		for _, id := range a {
			if !slices.ContainsFunc(groups, func(g Group) bool { return g.ID == id }) {
				return Selection{}, fmt.Errorf("script: the checklist %q has no group %q", c.Title, id)
			}
		}
		return selectionOf(groups), nil
	}
	return Selection{}, wrongAnswer(a, "checklist", c.Title)
}

// Menu answers with the next option Value.
func (s *Script) Menu(m Menu) (string, error) {
	a, err := s.next("menu", m.Title)
	if err != nil {
		return "", err
	}
	switch a := a.(type) {
	case useDefault:
		return Defaults{}.Menu(m)
	case string:
		for _, o := range m.Options {
			if o.Value == a {
				return a, nil
			}
		}
		return "", fmt.Errorf("script: the menu %q has no option %q", m.Title, a)
	}
	return "", wrongAnswer(a, "menu", m.Title)
}

// Confirm answers with the next bool.
func (s *Script) Confirm(c Confirm) (bool, error) {
	a, err := s.next("confirm", c.Title)
	if err != nil {
		return false, err
	}
	switch a := a.(type) {
	case useDefault:
		return Defaults{}.Confirm(c)
	case bool:
		return a, nil
	}
	return false, wrongAnswer(a, "confirm", c.Title)
}

// Input answers with the next string.
func (s *Script) Input(i Input) (string, error) {
	a, err := s.next("input", i.Title)
	if err != nil {
		return "", err
	}
	switch a := a.(type) {
	case useDefault:
		return Defaults{}.Input(i)
	case string:
		return a, scriptValidate(a, i.Validate, i.Title)
	}
	return "", wrongAnswer(a, "input", i.Title)
}

// Password answers with the next string.
func (s *Script) Password(p Password) (string, error) {
	a, err := s.next("password", p.Title)
	if err != nil {
		return "", err
	}
	switch a := a.(type) {
	case useDefault:
		return Defaults{}.Password(p)
	case string:
		return a, scriptValidate(a, p.Validate, p.Title)
	}
	return "", wrongAnswer(a, "password", p.Title)
}

func scriptValidate(a string, v func(string) error, title string) error {
	if v == nil {
		return nil
	}
	if err := v(a); err != nil {
		return fmt.Errorf("script: answer %q to %q isn't valid: %w", a, title, err)
	}
	return nil
}
