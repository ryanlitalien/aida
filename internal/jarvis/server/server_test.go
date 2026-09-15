package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer mounts the Jarvis routes with a nil Assistant. handleAsk no
// longer touches s.a (it 410s before any dispatch) and handleIndex only
// reads Server's own lastQuery/lastReply fields, so nil is safe here and
// keeps these tests free of heavyweight jarvis.Assistant fixtures (LLM
// client, brain DB, TTS, ...).
func newTestServer() *http.ServeMux {
	mux := http.NewServeMux()
	New(mux, nil)
	return mux
}

// TestAskReturnsGone verifies /jarvis/ask is decommissioned: it must return
// 410 Gone rather than run the LLM/tool-dispatch turn it used to. This
// endpoint was reachable via CSRF from any site the user visits, since
// loopback binding is not an auth boundary against a browser.
func TestAskReturnsGone(t *testing.T) {
	mux := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/jarvis/ask", strings.NewReader(`{"query":"hello"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusGone)
	}
}

// TestAskGoneWithoutBody confirms the handler short-circuits to 410 before
// ever touching the request body - a missing or garbage body must not
// produce a 400 (that would mean the old decode-then-validate path is still
// running ahead of the disable).
func TestAskGoneWithoutBody(t *testing.T) {
	cases := []struct {
		name string
		body io.Reader
	}{
		{"nil body", nil},
		{"garbage body", strings.NewReader("not json at all {{{")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := newTestServer()
			req := httptest.NewRequest(http.MethodPost, "/jarvis/ask", tc.body)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusGone {
				t.Fatalf("status = %d, want %d (got %d would mean it's still trying to decode)", rec.Code, http.StatusGone, rec.Code)
			}
		})
	}
}

// TestIndexHasNoAskForm guards against the "disabled route, live form"
// regression: the ask form must be gone from the status panel (a button
// that always 410s is worse than no button), while unrelated UI - the
// activity polling card - must still be present.
func TestIndexHasNoAskForm(t *testing.T) {
	mux := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()

	if strings.Contains(body, "askForm") {
		t.Error("index body still contains askForm")
	}
	if strings.Contains(body, "/jarvis/ask") {
		t.Error("index body still references /jarvis/ask")
	}

	if !strings.Contains(body, "pollActivity") {
		t.Error("index body no longer contains activity polling (pollActivity)")
	}
	if !strings.Contains(body, "/jarvis/activity") {
		t.Error("index body no longer references /jarvis/activity")
	}
}
