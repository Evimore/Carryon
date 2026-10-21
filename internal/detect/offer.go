package detect

import (
	"context"
	"fmt"
)

// offer is the hook's step for restoring as you go: it offers the saved settings of the apps just
// installed and prints a hint for each offer. It runs after the refresh, inside the hook's time
// limit, and what goes wrong becomes a hint. See docs/design.md, "As you go".
func offer(ctx context.Context, opt HookOptions, out *hintWriter) {
	if opt.Offer == nil || ctx.Err() != nil {
		return
	}
	hints, err := safeOffer(ctx, opt.Offer)
	if ctx.Err() != nil {
		return // Hook says so
	}
	for _, h := range hints {
		out.hint("%s", h)
	}
	if err != nil {
		out.hint("Carryon couldn't check for saved settings of what you installed: %v. Your packages are fine.", err)
	}
}

// safeOffer runs the offer step, turning a panic into an error.
func safeOffer(ctx context.Context, offer func(context.Context) ([]string, error)) (hints []string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("a bug stopped it: %v", p)
		}
	}()
	return offer(ctx)
}
