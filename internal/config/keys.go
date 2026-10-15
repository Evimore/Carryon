package config

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Keys lists every setting as a dotted key ("backup.after_change_delay"), in file order.
func Keys() []string {
	var keys []string
	t := reflect.TypeFor[Config]()
	for i := range t.NumField() {
		f := t.Field(i)
		name := tomlName(f)
		if f.Type.Kind() != reflect.Struct {
			keys = append(keys, name)
			continue
		}
		for j := range f.Type.NumField() {
			keys = append(keys, name+"."+tomlName(f.Type.Field(j)))
		}
	}
	return keys
}

// Get returns a setting as text: strings as they are, everything else as written in the file
// (lists as TOML arrays, which Set accepts back).
func (c Config) Get(key string) (string, error) {
	v, err := setting(reflect.ValueOf(&c).Elem(), key)
	if err != nil {
		return "", err
	}
	if v.Kind() == reflect.String {
		return v.String(), nil
	}
	return literal(v), nil
}

// Set changes one setting, reading value for the setting's type: a whole number, a number, true or
// false, or for lists either comma-separated items ("/srv, /opt/app") or a TOML array
// (["/srv", "/opt/app"]). An empty value makes an empty list. Set doesn't validate the result;
// call Validate.
func (c *Config) Set(key, value string) error {
	if key == "config_version" {
		return fmt.Errorf("config_version is managed by Carryon and can't be set")
	}
	v, err := setting(reflect.ValueOf(c).Elem(), key)
	if err != nil {
		return err
	}
	s := strings.TrimSpace(value)
	switch v.Kind() {
	case reflect.String:
		v.SetString(value)
	case reflect.Int:
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("%s needs a whole number, like 7; got %q", key, value)
		}
		v.SetInt(int64(n))
	case reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%s needs a number, like 5 or 2.5; got %q", key, value)
		}
		v.SetFloat(f)
	case reflect.Bool:
		switch strings.ToLower(s) {
		case "true", "yes", "on", "1":
			v.SetBool(true)
		case "false", "no", "off", "0":
			v.SetBool(false)
		default:
			return fmt.Errorf("%s needs true or false; got %q", key, value)
		}
	case reflect.Slice:
		list, err := parseList(s)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		v.Set(reflect.ValueOf(list))
	default:
		return fmt.Errorf("%s can't be set this way", key)
	}
	return nil
}

// parseList reads a list given as a TOML array or as comma-separated items.
func parseList(s string) ([]string, error) {
	if strings.HasPrefix(s, "[") {
		var doc struct{ V []string }
		if _, err := toml.Decode("v = "+s, &doc); err != nil {
			return nil, fmt.Errorf("%q isn't a list of quoted strings, like [\"/srv\", \"/opt/app\"]", s)
		}
		if doc.V == nil {
			doc.V = []string{}
		}
		return doc.V, nil
	}
	list := []string{}
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}
	return list, nil
}

// setting finds the field for a dotted key in a Config value.
func setting(cfg reflect.Value, key string) (reflect.Value, error) {
	section, name, dotted := strings.Cut(key, ".")
	v := cfg
	for _, part := range []string{section, name} {
		f, ok := fieldByTOML(v, part)
		if !ok {
			return reflect.Value{}, fmt.Errorf("unknown setting %q", key)
		}
		v = f
		if !dotted {
			break
		}
	}
	if v.Kind() == reflect.Struct {
		return reflect.Value{}, fmt.Errorf("%q is a section; name one of its settings, like %s.%s", key, key, tomlName(v.Type().Field(0)))
	}
	return v, nil
}

func fieldByTOML(v reflect.Value, name string) (reflect.Value, bool) {
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	for i := range v.NumField() {
		if tomlName(v.Type().Field(i)) == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func tomlName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
	return name
}

// literal formats a setting's value as a TOML value.
func literal(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		return quote(v.String())
	case reflect.Int:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Slice:
		items := make([]string, v.Len())
		for i := range items {
			items[i] = quote(v.Index(i).String())
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	return fmt.Sprintf("%v", v.Interface())
}

// quote writes s as a TOML basic string.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
