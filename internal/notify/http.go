package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPTimeout is how long one ntfy or webhook request may take, start to finish.
const HTTPTimeout = 15 * time.Second

// newClient is the HTTP client senders use when none is given.
func newClient() *http.Client { return &http.Client{Timeout: HTTPTimeout} }

// Ntfy pushes messages to an ntfy topic (https://ntfy.sh or a self-hosted server). Topics are often
// public, so the title and body are redacted before sending.
type Ntfy struct {
	URL    string       // the topic's URL, such as https://ntfy.sh/my-topic
	Redact Redactor     // hides secrets; the default rules always apply
	Client *http.Client // nil means a client with HTTPTimeout
}

// Name is "ntfy (HOST)": the topic itself is left out, since anyone who knows it can read it.
func (n Ntfy) Name() string { return "ntfy (" + hostOf(n.URL) + ")" }

// Send posts m as one ntfy message, with its title, priority and tags as headers.
func (n Ntfy) Send(ctx context.Context, m Message) (string, error) {
	m = n.Redact.redactMessage(m)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(m.Body))
	if err != nil {
		return "", fmt.Errorf("notify.ntfy_url isn't a usable address: %s", Redact(err.Error()))
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", headerText(m.Title))
	tags := []string{"information_source"}
	priority := "default"
	if m.Urgency == Critical {
		tags, priority = []string{"warning"}, "high"
	}
	if m.Host != "" {
		tags = append(tags, headerText(m.Host))
	}
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", strings.Join(tags, ","))
	if err := post(n.Client, req); err != nil {
		return "", err
	}
	return hostOf(n.URL), nil
}

// Webhook posts messages as a small JSON document to any URL, for chat bots and home automation.
type Webhook struct {
	URL    string
	Redact Redactor     // hides secrets; the default rules always apply
	Client *http.Client // nil means a client with HTTPTimeout
}

// Name is "webhook (HOST)": the path is left out, as it often holds a secret.
func (w Webhook) Name() string { return "webhook (" + hostOf(w.URL) + ")" }

// WebhookPayload is the JSON document a webhook receives.
type WebhookPayload struct {
	Title   string `json:"title"`
	Message string `json:"message"`
	Host    string `json:"host"`
	Unit    string `json:"unit"`
	Time    string `json:"time"`    // RFC 3339
	Urgency string `json:"urgency"` // "normal" or "critical"
}

// Send posts m as a WebhookPayload.
func (w Webhook) Send(ctx context.Context, m Message) (string, error) {
	m = w.Redact.redactMessage(m)
	body, err := json.Marshal(WebhookPayload{
		Title:   m.Title,
		Message: m.Body,
		Host:    m.Host,
		Unit:    m.Unit,
		Time:    m.when().Format(time.RFC3339),
		Urgency: m.Urgency.String(),
	})
	if err != nil {
		return "", fmt.Errorf("encoding the webhook message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("notify.webhook_url isn't a usable address: %s", Redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	if err := post(w.Client, req); err != nil {
		return "", err
	}
	return hostOf(w.URL), nil
}

// post sends req and checks the server accepted it. Errors name the server, never the full URL.
func post(c *http.Client, req *http.Request) error {
	if c == nil {
		c = newClient()
	}
	host := req.URL.Host
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("couldn't reach %s: %s", host, Redact(err.Error()))
	}
	defer resp.Body.Close()
	// Read a little of the answer, so the server's reason shows and the connection can be reused.
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := fmt.Sprintf("%s answered %s", host, resp.Status)
		if line := firstLine(string(answer)); line != "" {
			msg += ": " + Redact(line)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// hostOf returns the host of a URL, or "invalid address" if it has none.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid address"
	}
	return u.Host
}

// headerText makes s safe for an HTTP header: one line, and non-ASCII text encoded the way
// ntfy understands (RFC 2047).
func headerText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for _, c := range s {
		if c > 0x7e {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 200 {
		line = line[:200] + "..."
	}
	return strings.TrimSpace(line)
}
