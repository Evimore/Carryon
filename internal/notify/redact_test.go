package notify

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	// Built at run time so the secret scanner doesn't take test data for real keys.
	awsKey := "AKIA" + strings.Repeat("Q", 16)
	mixed := strings.Repeat("Ab1", 12)
	tests := []struct {
		name, in string
		gone     []string // must not appear in the output
		kept     []string // must still appear
	}{
		{"url password", "Fatal: unable to open repository at rest:https://alice:hunter2@example.com/repo",
			[]string{"hunter2"}, []string{"rest:https://***@example.com/repo"}},
		{"url user only is left alone", "sftp:alice@example.com:/srv/backup", nil, []string{"sftp:alice@example.com:/srv/backup"}},
		{"env password", "RESTIC_PASSWORD=hunter2 restic backup", []string{"hunter2"}, []string{"RESTIC_PASSWORD=***", "restic backup"}},
		{"cloud key", "AWS_SECRET_ACCESS_KEY=s3cretvalue is wrong", []string{"s3cretvalue"}, []string{"AWS_SECRET_ACCESS_KEY=***", "is wrong"}},
		{"colon form", "password: hunter2", []string{"hunter2"}, []string{"password: ***"}},
		{"quoted", `token="open sesame"`, []string{"open sesame"}, []string{"token=***"}},
		{"query parameter", "GET https://example.com/hook?token=abcd&x=1 failed", []string{"abcd"}, []string{"token=***&x=1"}},
		{"azure sas", "https://my-bucket.example.com/c?sv=2020&sig=abcdef", []string{"abcdef"}, []string{"sig=***"}},
		{"bearer", "Authorization: Bearer abc.def", []string{"abc.def"}, []string{"Authorization: ***"}},
		{"rclone on-the-fly remote", "rclone::s3,access_key_id=keyid,secret_access_key=keysecret:bucket",
			[]string{"keyid", "keysecret"}, nil},
		{"aws key id", "key " + awsKey + " denied", []string{awsKey}, []string{"key *** denied"}},
		{"long key-like string", "token " + mixed + " rejected", []string{mixed}, []string{"rejected"}},
		{"restic ids stay", "snapshot 4f1c2a9b8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a3928170 saved", nil,
			[]string{"4f1c2a9b8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a3928170"}},
		{"ordinary errors stay", "Fatal: wrong password or no key found", nil, []string{"Fatal: wrong password or no key found"}},
		{"signals stay", "restic failed (signal: killed)", nil, []string{"signal: killed"}},
		{"paths stay", "/home/alice/.local/share/Steam: permission denied", nil, []string{"/home/alice/.local/share/Steam: permission denied"}},
		{"multi-line keeps lines", "password:\nnext line", nil, []string{"password:\nnext line"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			for _, s := range tt.gone {
				if strings.Contains(got, s) {
					t.Errorf("Redact(%q) = %q, still contains %q", tt.in, got, s)
				}
			}
			for _, s := range tt.kept {
				if !strings.Contains(got, s) {
					t.Errorf("Redact(%q) = %q, want it to contain %q", tt.in, got, s)
				}
			}
		})
	}
}

func TestRedactorSecrets(t *testing.T) {
	r := Redactor{Secrets: []string{"correct horse\n", "abc", ""}}
	got := r.Redact("restic said: correct horse is not abc")
	if got != "restic said: *** is not abc" {
		t.Errorf("got %q (short secrets are ignored, the rest hidden)", got)
	}
}
