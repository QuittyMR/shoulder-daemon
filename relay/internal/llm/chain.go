package llm

import (
	"context"
	"errors"
	"strings"
)

// Chain tries each provider in order and returns the first success. It exists
// so a subscription-backed provider can lead and a metered one can cover its
// outages, without the decision step becoming a single point of failure.
type Chain struct {
	Providers  []Provider
	OnFallback func(from, to string, err error)
}

func (c *Chain) Name() string {
	names := make([]string, 0, len(c.Providers))
	for _, p := range c.Providers {
		names = append(names, p.Name())
	}
	return strings.Join(names, "→")
}

func (c *Chain) Complete(ctx context.Context, system, user string) (string, error) {
	var errs []error
	for i, p := range c.Providers {
		out, err := p.Complete(ctx, system, user)
		if err == nil {
			return out, nil
		}
		errs = append(errs, err)
		// A cancelled or expired parent context will fail every provider
		// identically; do not burn the fallbacks on it.
		if ctx.Err() != nil {
			break
		}
		if c.OnFallback != nil && i+1 < len(c.Providers) {
			c.OnFallback(p.Name(), c.Providers[i+1].Name(), err)
		}
	}
	return "", errors.Join(errs...)
}

func (c *Chain) Chat(ctx context.Context, msgs []Message, tools []Tool) (Message, error) {
	var errs []error
	for i, p := range c.Providers {
		out, err := p.Chat(ctx, msgs, tools)
		if err == nil {
			return out, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
		if c.OnFallback != nil && i+1 < len(c.Providers) {
			c.OnFallback(p.Name(), c.Providers[i+1].Name(), err)
		}
	}
	return Message{}, errors.Join(errs...)
}
