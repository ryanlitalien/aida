package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Guardrail validates either input or output text for an Agent. A
// non-nil error from Check fails the Runner early. Modeled on
// OpenAI Agents SDK guardrails, which execute in parallel with the
// main work and cancel on failure.
type Guardrail interface {
	Name() string
	Check(ctx context.Context, text string) error
}

// GuardrailFunc adapts a plain function into a Guardrail.
type GuardrailFunc struct {
	GName string
	GFn   func(ctx context.Context, text string) error
}

// Name implements Guardrail.
func (g *GuardrailFunc) Name() string { return g.GName }

// Check implements Guardrail.
func (g *GuardrailFunc) Check(ctx context.Context, text string) error {
	return g.GFn(ctx, text)
}

// GuardrailViolation is the error type returned when a guardrail
// fails. Callers can errors.As it to recover the guardrail name and
// the original underlying cause.
type GuardrailViolation struct {
	Guardrail string
	Err       error
}

// Error implements error.
func (g *GuardrailViolation) Error() string {
	return fmt.Sprintf("guardrail %q: %v", g.Guardrail, g.Err)
}

// Unwrap returns the underlying cause.
func (g *GuardrailViolation) Unwrap() error { return g.Err }

// runGuardrailsParallel runs every guardrail against text concurrently
// and returns the first violation. If any guardrail fails, all others
// are cancelled via the shared context - mirroring the OpenAI Agents
// SDK "fail fast" semantic.
func runGuardrailsParallel(ctx context.Context, guards []Guardrail, text string) error {
	if len(guards) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstViolation *GuardrailViolation

	for _, g := range guards {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Check(ctx, text); err != nil {
				mu.Lock()
				if firstViolation == nil && !errors.Is(err, context.Canceled) {
					firstViolation = &GuardrailViolation{Guardrail: g.Name(), Err: err}
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstViolation != nil {
		return firstViolation
	}
	return nil
}
