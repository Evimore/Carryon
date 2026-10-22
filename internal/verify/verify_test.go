package verify

import (
	"testing"
	"time"
)

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 49, 0, 0, time.UTC)
	tests := []struct {
		before time.Duration
		want   string
	}{
		{20 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{42 * time.Minute, "42 minutes ago"},
		{time.Hour, "1 hour ago"},
		{90 * time.Minute, "1 hour ago"},
		{5 * time.Hour, "5 hours ago"},
		{30 * time.Hour, "30 hours ago"},
		{72 * time.Hour, "3 days ago"},
	}
	for _, tt := range tests {
		if got := ago(now, now.Add(-tt.before)); got != tt.want {
			t.Errorf("ago(%s before) = %q, want %q", tt.before, got, tt.want)
		}
	}
}
