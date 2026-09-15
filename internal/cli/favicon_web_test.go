package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newFaviconTestMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerFaviconRoutes(mux)
	return mux
}

// TestFaviconICO checks the .ico route: 200, the right content type, and a
// non-empty body that actually starts with an ICO header (0x00 0x00 0x01
// 0x00) rather than e.g. an accidentally-swapped PNG.
func TestFaviconICO(t *testing.T) {
	mux := newFaviconTestMux()
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/x-icon" {
		t.Errorf("Content-Type = %q, want image/x-icon", ct)
	}
	body := rec.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("empty body")
	}
	want := []byte{0x00, 0x00, 0x01, 0x00}
	if len(body) < 4 || !bytes.Equal(body[:4], want) {
		t.Errorf("body does not start with ICO header %x, got %x", want, body[:min(4, len(body))])
	}
}

// TestFaviconSVG checks the .svg route serves image/svg+xml with content.
func TestFaviconSVG(t *testing.T) {
	mux := newFaviconTestMux()
	req := httptest.NewRequest(http.MethodGet, "/favicon.svg", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty body")
	}
}

// TestAppleTouchIcon checks /apple-touch-icon.png serves image/png with
// content (the 180x180 PNG).
func TestAppleTouchIcon(t *testing.T) {
	mux := newFaviconTestMux()
	req := httptest.NewRequest(http.MethodGet, "/apple-touch-icon.png", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty body")
	}
}

// TestFaviconIconSizes checks the three /icon/favicon-*.png routes all
// serve image/png with non-empty bodies.
func TestFaviconIconSizes(t *testing.T) {
	mux := newFaviconTestMux()
	for _, path := range []string{"/icon/favicon-32.png", "/icon/favicon-180.png", "/icon/favicon-512.png"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("%s: Content-Type = %q, want image/png", path, ct)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}
}

// TestSiteWebmanifest checks /site.webmanifest serves application/manifest+json
// with a body referencing the two PNG icon sizes.
func TestSiteWebmanifest(t *testing.T) {
	mux := newFaviconTestMux()
	req := httptest.NewRequest(http.MethodGet, "/site.webmanifest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("Content-Type = %q, want application/manifest+json", ct)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("favicon-180.png")) {
		t.Error("manifest body missing favicon-180.png reference")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("favicon-512.png")) {
		t.Error("manifest body missing favicon-512.png reference")
	}
}

// TestFaviconCacheControl checks the shared 24h cache header is set on a
// representative route.
func TestFaviconCacheControl(t *testing.T) {
	mux := newFaviconTestMux()
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want public, max-age=86400", cc)
	}
}

// TestFaviconPOSTNotAllowed checks the Go 1.22+ mux auto-405s a
// non-GET/HEAD method on a "GET " pattern.
func TestFaviconPOSTNotAllowed(t *testing.T) {
	mux := newFaviconTestMux()
	req := httptest.NewRequest(http.MethodPost, "/favicon.ico", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestMenuLANMuxServesFavicon exercises newMenuLANMux (serve.go), the mux
// startMenuLANServer binds to 0.0.0.0 for the kids'-phone menu listener --
// confirming the favicon routes are reachable there too, not just on the
// main loopback daemon's mux.
func TestMenuLANMuxServesFavicon(t *testing.T) {
	mux := newMenuLANMux("/nonexistent/path/menu.yaml")

	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/x-icon" {
		t.Errorf("Content-Type = %q, want image/x-icon", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty body")
	}
}
