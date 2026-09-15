package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// validOpenAIResponse returns a minimal but valid OpenAI chat completions JSON response.
func validOpenAIResponse() string {
	resp := openaiResponse{
		Choices: []openaiChoice{
			{Message: openaiMessage{Role: "assistant", Content: "Hello from OpenAI!"}},
		},
		Usage: openaiUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
		},
		Model: "gpt-4o-mini",
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

func TestOpenAIRetryOnTransientError(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			// First two calls: 429 Too Many Requests
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		// Third call: success
		w.WriteHeader(200)
		w.Write([]byte(validOpenAIResponse()))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL)
	ctx := context.Background()

	resp, err := p.Complete(ctx, Request{
		Model:      "gpt-4o-mini",
		UserPrompt: "hello",
	})
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}
	if resp.Text != "Hello from OpenAI!" {
		t.Errorf("unexpected response text: %q", resp.Text)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 total calls (1 original + 2 retries), got %d", got)
	}
	if resp.DurationMs <= 0 {
		t.Error("expected positive DurationMs")
	}
}

func TestOpenAIRetryExhausted(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"service unavailable"}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL)
	ctx := context.Background()

	_, err := p.Complete(ctx, Request{
		Model:      "gpt-4o-mini",
		UserPrompt: "hello",
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 total attempts (1 original + 2 retries), got %d", got)
	}
}

func TestOpenAINonTransientError(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("bad-key", srv.URL)
	ctx := context.Background()

	_, err := p.Complete(ctx, Request{
		Model:      "gpt-4o-mini",
		UserPrompt: "hello",
	})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 call (no retry on 401), got %d", got)
	}
}

func TestOpenAIRetryDelay(t *testing.T) {
	tests := []struct {
		attempt int
		minMs   int64
		maxMs   int64
	}{
		{0, 375, 500},   // 500ms base, minus up to 25% jitter → [375, 500]
		{1, 750, 1000},  // 1000ms, minus up to 25% jitter → [750, 1000]
		{2, 1500, 2000}, // 2000ms, minus up to 25% jitter → [1500, 2000]
		{3, 3000, 4000}, // 4000ms, minus up to 25% jitter → [3000, 4000]
		{4, 6000, 8000}, // 8000ms (capped), minus up to 25% jitter → [6000, 8000]
		{5, 6000, 8000}, // still capped at 8000ms → [6000, 8000]
	}

	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			// Run multiple times to account for jitter randomness
			for i := 0; i < 50; i++ {
				d := openaiRetryDelay(tc.attempt)
				ms := d.Milliseconds()
				if ms < tc.minMs || ms > tc.maxMs {
					t.Errorf("attempt %d: delay %dms outside expected range [%d, %d]",
						tc.attempt, ms, tc.minMs, tc.maxMs)
					break
				}
			}
		})
	}
}

func TestIsTransientHTTPError(t *testing.T) {
	tests := []struct {
		code      int
		transient bool
	}{
		{200, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{408, true}, // Request Timeout
		{409, true}, // Conflict
		{429, true}, // Too Many Requests
		{500, true}, // Internal Server Error
		{502, true}, // Bad Gateway
		{503, true}, // Service Unavailable
		{504, true}, // Gateway Timeout
		{522, true}, // Cloudflare timeout
	}

	for _, tc := range tests {
		got := isTransientHTTPError(tc.code)
		if got != tc.transient {
			t.Errorf("isTransientHTTPError(%d) = %v, want %v", tc.code, got, tc.transient)
		}
	}
}

func TestOpenAIRetryRespectsContextCancellation(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"service unavailable"}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Complete(ctx, Request{
		Model:      "gpt-4o-mini",
		UserPrompt: "hello",
	})
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	// Should have fewer than 3 calls because context should cancel during backoff
	if got := calls.Load(); got >= 3 {
		t.Errorf("expected fewer than 3 calls due to context cancellation, got %d", got)
	}
}
