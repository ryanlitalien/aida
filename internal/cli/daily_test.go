package cli

import (
	"context"
	"testing"
	"time"
)

func TestWaitForNetwork_Success(t *testing.T) {
	// localhost always resolves; should return nil on the first attempt
	// with no measurable wait.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := waitForNetwork(ctx, "localhost", 5*time.Second); err != nil {
		t.Fatalf("waitForNetwork(localhost) = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("localhost resolve took %v, expected near-instant", elapsed)
	}
}

func TestWaitForNetwork_UnresolvableHost(t *testing.T) {
	// .invalid is reserved by RFC 2606 and will never resolve. Budget is
	// short to keep the test fast; we just verify the timeout path returns
	// an error rather than hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := waitForNetwork(ctx, "definitely-not-a-real-host.invalid", 100*time.Millisecond)
	if err == nil {
		t.Fatal("waitForNetwork(unresolvable) = nil, want error")
	}
}

func TestWaitForNetwork_ContextCancel(t *testing.T) {
	// Caller cancels mid-wait → must return ctx.Err() rather than spinning
	// to the budget deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	err := waitForNetwork(ctx, "definitely-not-a-real-host.invalid", 10*time.Second)
	if err == nil {
		t.Fatal("waitForNetwork on cancelled ctx = nil, want error")
	}
}
