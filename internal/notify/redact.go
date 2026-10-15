package notify

import (
	"regexp"
	"strings"
	"unicode"
)

// hidden replaces redacted text.
const hidden = "***"

var (
	// user:password@ in a URL, such as rest:https://alice:pw@example.com/ or https://alice:pw@ntfy.sh.
	urlUserinfoRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/@:]*:[^\s/@]*@`)
	// "Bearer abc" or "Basic abc" (Authorization headers echoed in errors).
	bearerRE = regexp.MustCompile(`(?i)\b(bearer|basic)[ \t]+[A-Za-z0-9._~+/=-]+`)
	// key=value or key: value where the key names a secret, including environment variables such as
	// RESTIC_PASSWORD=... or AWS_SECRET_ACCESS_KEY=... and query parameters such as ?token=...
	secretKVRE = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:password|passwd|passphrase|secret|token|api[_-]?key|access[_-]?key|account[_-]?key|private[_-]?key|credentials?|authorization|signature)[A-Za-z0-9_.-]*)([ \t]*[:=][ \t]*)("[^"]*"|'[^']*'|[^\s&"']+)`)
	// Azure's shared access signature in a query string.
	sigParamRE = regexp.MustCompile(`([?&]sig=)[^\s&]+`)
	// AWS access key IDs.
	awsKeyRE = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	// Long runs of characters that could be a key or token. Only those mixing upper case, lower
	// case and digits are hidden, so snapshot and repository IDs (lower-case hex) and paths stay.
	longTokenRE = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)
)

// Redactor hides secrets in text before it leaves the machine.
type Redactor struct {
	// Secrets are exact strings to hide wherever they appear, such as the repository password.
	// Strings shorter than 4 characters are ignored, so they can't wipe out ordinary words.
	Secrets []string
}

// Redact hides user:password@ in URLs and anything that looks like a password, token or key,
// with no exact secrets known. See Redactor.Redact.
func Redact(s string) string { return Redactor{}.Redact(s) }

// Redact returns s with r.Secrets, the passwords in URLs, key=value pairs naming a secret,
// Authorization tokens and long key-like strings replaced by "***". It errs on hiding too much.
func (r Redactor) Redact(s string) string {
	for _, secret := range r.Secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) >= 4 {
			s = strings.ReplaceAll(s, secret, hidden)
		}
	}
	s = urlUserinfoRE.ReplaceAllString(s, "${1}"+hidden+"@")
	s = bearerRE.ReplaceAllString(s, "${1} "+hidden)
	s = secretKVRE.ReplaceAllString(s, "${1}${2}"+hidden)
	s = sigParamRE.ReplaceAllString(s, "${1}"+hidden)
	s = awsKeyRE.ReplaceAllString(s, hidden)
	s = longTokenRE.ReplaceAllStringFunc(s, func(t string) string {
		if keyLike(t) {
			return hidden
		}
		return t
	})
	return s
}

// keyLike reports whether t mixes upper case, lower case and digits, as generated keys do.
func keyLike(t string) bool {
	var upper, lower, digit bool
	for _, c := range t {
		switch {
		case unicode.IsUpper(c):
			upper = true
		case unicode.IsLower(c):
			lower = true
		case unicode.IsDigit(c):
			digit = true
		}
	}
	return upper && lower && digit
}

// redactMessage returns m with every text field redacted.
func (r Redactor) redactMessage(m Message) Message {
	m.Title = r.Redact(m.Title)
	m.Body = r.Redact(m.Body)
	m.Unit = r.Redact(m.Unit)
	m.Host = r.Redact(m.Host)
	return m
}
