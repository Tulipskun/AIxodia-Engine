package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"aixodia-engine/provider"
)

// A provider that fails repeatedly is paused for a while so every chat does not
// spend its retry budget on it first. Only upstream-side failures count: a
// rejected key or a bad request says nothing about whether the provider is up.
const (
	breakerThreshold = 3
	breakerCooldown  = 45 * time.Second
)

type breaker struct {
	failures  int
	openUntil time.Time
}

func (g *Gateway) circuitOpen(name string, now time.Time) bool {
	g.brMu.Lock()
	defer g.brMu.Unlock()
	b := g.breakers[name]
	return b != nil && now.Before(b.openUntil)
}

func (g *Gateway) recordOutcome(name string, err error, now time.Time) {
	g.brMu.Lock()
	defer g.brMu.Unlock()
	b := g.breakers[name]
	if b == nil {
		b = &breaker{}
		g.breakers[name] = b
	}
	if err == nil {
		b.failures = 0
		b.openUntil = time.Time{}
		return
	}
	if !countsAgainstProvider(err) {
		return
	}
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = now.Add(breakerCooldown)
		b.failures = 0
	}
}

func countsAgainstProvider(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var api *provider.APIError
	if errors.As(err, &api) {
		return api.StatusCode == http.StatusTooManyRequests ||
			api.StatusCode == http.StatusRequestTimeout ||
			api.StatusCode >= 500
	}
	return true
}
