package cli

import (
	"embed"
	"net/http"
)

// faviconAssetsFS holds the A.I.D.A. round launcher icon in every size and
// format browsers and phones ask for: the legacy favicon.ico, a modern
// favicon.svg, three PNG sizes for manifest/touch-icon use, and the
// apple-touch-icon for iOS home-screen bookmarks. Mirrors the precedent in
// dashboard_web.go, which embeds assets/arrow the same way.
//
// Source: internal/cli/assets/icon/favicon.svg, derived from
// aida-android's ic_launcher_round.
//
//go:embed assets/icon
var faviconAssetsFS embed.FS

// faviconAsset reads one file out of faviconAssetsFS, panicking on failure.
// Like the arrow embed in dashboard_web.go, a miss here is only reachable
// if the //go:embed directive or the assets/icon directory itself is
// broken -- a build-time invariant, never something user input can trigger.
func faviconAsset(name string) []byte {
	b, err := faviconAssetsFS.ReadFile("assets/icon/" + name)
	if err != nil {
		panic("favicon: bad icon asset embed: " + err.Error())
	}
	return b
}

// siteWebmanifest is the PWA manifest for the /menu home-screen bookmark:
// "A.I.D.A." branding, the two PNG sizes above, and a start_url landing
// straight on /menu, the only page meant for a home-screen icon.
const siteWebmanifest = `{
  "name": "A.I.D.A.",
  "short_name": "Aida",
  "icons": [
    {"src": "/icon/favicon-180.png", "sizes": "180x180", "type": "image/png"},
    {"src": "/icon/favicon-512.png", "sizes": "512x512", "type": "image/png"}
  ],
  "theme_color": "#0B1220",
  "background_color": "#0B1220",
  "display": "standalone",
  "start_url": "/menu"
}
`

// registerFaviconRoutes registers the favicon/touch-icon/manifest routes
// shared by every page `aida serve` hosts, on both the main loopback mux
// and the menu LAN mux (see startMenuLANServer in serve.go). Deliberately
// unguarded (no guardLocalHost): these are static, harmless assets, and the
// whole point of the LAN mux is being reachable from a kid's phone that is
// not loopback.
//
// GET-only patterns ("GET /path") already make the Go 1.22+ stdlib mux
// auto-405 any other method, including HEAD's sibling verbs, while still
// answering HEAD requests correctly (net/http strips the body for HEAD
// automatically), so nothing extra is needed to satisfy "GET/HEAD only".
func registerFaviconRoutes(mux *http.ServeMux) {
	serve := func(pattern, contentType string, body []byte) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(body)
		})
	}

	serve("GET /favicon.ico", "image/x-icon", faviconAsset("favicon.ico"))
	serve("GET /favicon.svg", "image/svg+xml", faviconAsset("favicon.svg"))
	serve("GET /icon/favicon-32.png", "image/png", faviconAsset("favicon-32.png"))
	serve("GET /icon/favicon-180.png", "image/png", faviconAsset("favicon-180.png"))
	serve("GET /icon/favicon-512.png", "image/png", faviconAsset("favicon-512.png"))
	serve("GET /apple-touch-icon.png", "image/png", faviconAsset("favicon-180.png"))
	serve("GET /site.webmanifest", "application/manifest+json", []byte(siteWebmanifest))
}
