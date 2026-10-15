package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorded is one request a test server received.
type recorded struct {
	header http.Header
	body   string
	user   string
	pass   string
}

func server(t *testing.T, status int) (*httptest.Server, *[]recorded) {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user, pass, _ := r.BasicAuth()
		got = append(got, recorded{header: r.Header.Clone(), body: string(body), user: user, pass: pass})
		w.WriteHeader(status)
		io.WriteString(w, "topic is read-only\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

var failed = Message{
	Title:   "Backup failed",
	Body:    "unable to open repository at rest:https://alice:hunter2@example.com/\nDetails: carryon status",
	Urgency: Critical,
	Unit:    "carryon-backup.service",
	Host:    "desktop",
	Time:    time.Date(2026, 10, 15, 3, 4, 5, 0, time.UTC),
}

func TestNtfy(t *testing.T) {
	srv, got := server(t, http.StatusOK)
	n := Ntfy{URL: srv.URL + "/my-topic", Redact: Redactor{Secrets: []string{"correct horse"}}}
	m := failed
	m.Body += "\npassword was correct horse"
	to, err := n.Send(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(to, "127.0.0.1:") || strings.Contains(n.Name(), "my-topic") {
		t.Errorf("to = %q, name = %q: neither should show the topic", to, n.Name())
	}
	r := (*got)[0]
	if r.header.Get("Title") != "Backup failed" || r.header.Get("Priority") != "high" || r.header.Get("Tags") != "warning,desktop" {
		t.Errorf("headers = %v", r.header)
	}
	for _, secret := range []string{"hunter2", "correct horse"} {
		if strings.Contains(r.body, secret) {
			t.Errorf("body sent to ntfy contains %q: %q", secret, r.body)
		}
	}
	if !strings.Contains(r.body, "Details: carryon status") {
		t.Errorf("body = %q", r.body)
	}

	// a test message is quieter
	if _, err := n.Send(context.Background(), Test("desktop")); err != nil {
		t.Fatal(err)
	}
	if p := (*got)[1].header.Get("Priority"); p != "default" {
		t.Errorf("test priority = %q", p)
	}
}

func TestNtfyLoginInURL(t *testing.T) {
	srv, got := server(t, http.StatusOK)
	u := strings.Replace(srv.URL, "http://", "http://alice:hunter2@", 1) + "/my-topic"
	n := Ntfy{URL: u}
	if strings.Contains(n.Name(), "hunter2") {
		t.Errorf("name = %q", n.Name())
	}
	if _, err := n.Send(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	if r := (*got)[0]; r.user != "alice" || r.pass != "hunter2" {
		t.Errorf("basic auth = %q %q, want the login from the URL", r.user, r.pass)
	}
}

func TestNtfyNonASCIITitle(t *testing.T) {
	srv, got := server(t, http.StatusOK)
	m := failed
	m.Title = "Sauvegarde échouée"
	if _, err := (Ntfy{URL: srv.URL + "/t"}).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if h := (*got)[0].header.Get("Title"); !strings.HasPrefix(h, "=?utf-8?q?") {
		t.Errorf("Title = %q, want RFC 2047 encoding", h)
	}
}

func TestWebhook(t *testing.T) {
	srv, got := server(t, http.StatusNoContent)
	w := Webhook{URL: srv.URL + "/hooks/abc123"}
	if strings.Contains(w.Name(), "abc123") {
		t.Errorf("name = %q shows the path", w.Name())
	}
	if _, err := w.Send(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var p WebhookPayload
	if err := json.Unmarshal([]byte(r.body), &p); err != nil {
		t.Fatal(err)
	}
	want := WebhookPayload{
		Title:   "Backup failed",
		Message: "unable to open repository at rest:https://***@example.com/\nDetails: carryon status",
		Host:    "desktop", Unit: "carryon-backup.service", Time: "2026-10-15T03:04:05Z", Urgency: "critical",
	}
	if p != want {
		t.Errorf("payload = %+v\nwant      %+v", p, want)
	}
}

func TestHTTPFailures(t *testing.T) {
	srv, _ := server(t, http.StatusForbidden)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) }) // runs first
	quick := &http.Client{Timeout: 100 * time.Millisecond}

	tests := []struct {
		name string
		s    Sender
		want string
	}{
		{"refused", Ntfy{URL: srv.URL + "/my-topic"}, "403 Forbidden: topic is read-only"},
		{"webhook refused", Webhook{URL: srv.URL + "/x"}, "403 Forbidden"},
		{"bad url", Webhook{URL: "http://exa mple.com/"}, "isn't a usable address"},
		{"timeout", Ntfy{URL: slow.URL + "/my-topic", Client: quick}, "couldn't reach"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			_, err := tt.s.Send(context.Background(), failed)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "my-topic") {
				t.Errorf("err = %v shows the topic", err)
			}
			if time.Since(start) > 3*time.Second {
				t.Errorf("took %s", time.Since(start))
			}
		})
	}
}
