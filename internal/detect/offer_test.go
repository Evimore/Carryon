package detect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/state"
)

// The offer runs after the refresh, and its hints and problems show in pacman's output.
func TestHookOffers(t *testing.T) {
	var out syncBuf
	var order []string
	var recorded []state.Run
	Hook(context.Background(), HookOptions{
		Refresh: func(context.Context) ([]string, error) { order = append(order, "refresh"); return nil, nil },
		Offer: func(context.Context) ([]string, error) {
			order = append(order, "offer")
			return []string{"carryon: saved settings for obs-studio (9 Oct) — restore with: carryon restore --bundle app-obs-studio"},
				errors.New("the inbox is busy")
		},
		Record: func(r state.Run) { recorded = append(recorded, r) },
		Out:    &out,
	})
	if strings.Join(order, ",") != "refresh,offer" {
		t.Errorf("order = %v", order)
	}
	for _, want := range []string{
		"carryon: saved settings for obs-studio (9 Oct) — restore with: carryon restore --bundle app-obs-studio",
		"Carryon couldn't check for saved settings of what you installed: the inbox is busy. Your packages are fine.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("hints lack %q:\n%s", want, out.String())
		}
	}
	if len(recorded) != 1 || recorded[0].Result != state.ResultOK {
		t.Errorf("an offer's problem changed the capture's run: %+v", recorded)
	}
}
