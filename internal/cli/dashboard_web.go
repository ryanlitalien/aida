package cli

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/bifrost"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/fleet"
	"github.com/ryanlitalien/aida/internal/jarvis/audio"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/remotex"
	"github.com/ryanlitalien/aida/internal/runs"
)

func init() {
	// Go's builtin mime table maps .mjs -> text/javascript on most
	// platforms, but the mime package also consults the OS's
	// /etc/mime.types (and friends) at init time, and that file can
	// override the mapping to something a browser refuses to load as an
	// ES module (or leave it unset, in which case ServeContent falls
	// back to content sniffing). Force it explicitly rather than trust
	// the ambient environment -- TestArrowAssetServes exists specifically
	// to catch a regression here, because a wrong Content-Type makes the
	// dashboard/bifrost page silently render nothing.
	_ = mime.AddExtensionType(".mjs", "text/javascript")
}

//go:embed dashboard_web.html
var dashboardWebHTML []byte

//go:embed bifrost_web.html
var bifrostWebHTML []byte

// arrowAssetsFS holds the vendored arrow (reactive HTML templating)
// library that dashboard_web.html and bifrost_web.html import as an ES
// module from /static/arrow/index.mjs. See internal/cli/assets/arrow/.
//
//go:embed assets/arrow
var arrowAssetsFS embed.FS

// dashboardDeps is everything registerDashboardRoutes needs to serve the
// /dashboard and /bifrost pages. Bifrost is empty (len == 0) when no
// configured device sets herdr: true -- every /api/bifrost/* handler must
// degrade gracefully in that case (see registerBifrostRoutes).
type dashboardDeps struct {
	Cfg     *config.Config
	Brain   *brain.Brain
	Jobs    *jobs.Store
	Profile string
	Fleet   *fleet.Cache
	// Bifrost holds one Client per configured /bifrost lane, keyed by
	// lane user. BifrostLaneOrder carries the configured lane order
	// separately because map iteration order is random and index 0 of
	// that slice is the default lane an unqualified request selects.
	Bifrost          map[string]*bifrost.Client
	BifrostLaneOrder []string
	Machine          string
	Started          time.Time
	// ModelsPath is the AI provider/plan/nickname roster YAML
	// (config.Config.ModelsPath) the Models panel's GET /api/models reads.
	ModelsPath string
}

// newDashboardDeps assembles the dashboard/bifrost dependencies from
// config. It lives here rather than in serve.go so that wiring this
// feature into the daemon stays a few lines: runHTTPDaemon is a long
// function that another branch is actively editing, and every line added
// there is a line to reconcile at merge time.
//
// Returns the fleet cache separately because the caller needs it to start
// the background refresh ticker against the daemon's signal context; it is
// the same pointer carried in deps.Fleet.
//
// The fleet.Cache return value is never nil. A machine with no `devices:`
// block still gets a working one-node dashboard via ResolveDevices, and a
// missing herdr device yields an empty deps.Bifrost map, which every
// /api/bifrost/* handler degrades on rather than 404s.
func newDashboardDeps(cfg *config.Config, b *brain.Brain, jobsStore *jobs.Store, profileName string, started time.Time) (dashboardDeps, *fleet.Cache) {
	machine := audio.MachineName()
	devices := cfg.ResolveDevices(machine)

	// Validation is non-fatal on purpose: a typo in devices:/fleet: must
	// not stop `aida serve` booting the voice loop. Warn, and keep
	// serving. `aida lint` is where this would be an error.
	if errs := cfg.ValidateDevices(); len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintf(os.Stderr, "config: %v\n", err)
		}
	}
	if errs := cfg.ValidateFleet(); len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintf(os.Stderr, "config: %v\n", err)
		}
	}

	prober := &fleet.Prober{Runner: remotex.ExecRunner{}}
	cache := fleet.NewCache(prober, devices, cfg.Fleet)

	bifrostClients, bifrostLaneOrder := newBifrostClients(cfg, devices)

	return dashboardDeps{
		Cfg:              cfg,
		Brain:            b,
		Jobs:             jobsStore,
		Profile:          profileName,
		Fleet:            cache,
		Bifrost:          bifrostClients,
		BifrostLaneOrder: bifrostLaneOrder,
		Machine:          machine,
		Started:          started,
		ModelsPath:       cfg.ModelsPath(),
	}, cache
}

// newBifrostClients builds one bifrost.Client per configured /bifrost
// lane (cfg.Fleet.BifrostLanes), all against the single herdr: true
// device -- both lanes live on the same box (minty), just as different
// sudo users. A lane whose Client fails to construct (e.g. a preset
// misassigned by presetsForLane, or an invalid username) is dropped with
// a stderr warning rather than taking down the daemon or the other
// lanes; the /bifrost page's per-lane error surfaces the reason.
//
// The returned slice is the subset of BifrostLanes() that built
// successfully, in the same order -- index 0 is the default lane.
func newBifrostClients(cfg *config.Config, devices []config.DeviceConfig) (map[string]*bifrost.Client, []string) {
	clients := map[string]*bifrost.Client{}
	var order []string

	var herdrHost string
	for _, d := range devices {
		if d.Herdr {
			herdrHost = d.SSHTarget()
			break
		}
	}
	if herdrHost == "" {
		return clients, order
	}

	for i, laneUser := range cfg.Fleet.BifrostLanes() {
		presets := bifrostPresetsFromConfig(presetsForLane(cfg.Bifrost.Presets, laneUser, i == 0))
		client, err := bifrost.New(remotex.ExecRunner{}, herdrHost, laneUser, presets)
		if err != nil {
			// A malformed lane disables that lane's remote calls but must
			// not take down the daemon or the other lanes; the page shows
			// the reason via the per-lane error map.
			fmt.Fprintf(os.Stderr, "bifrost: lane %q: %v (the /bifrost page will show this)\n", laneUser, err)
			continue
		}
		clients[laneUser] = client
		order = append(order, laneUser)
	}
	return clients, order
}

// presetsForLane filters cfg.Bifrost.Presets down to the ones assigned to
// laneUser: an explicit User match, or -- when isFirstLane -- an unset
// User ("empty = first lane," per the brief). The returned presets carry
// an explicit, resolved User even when the config left it blank, so
// bifrost.Client.Presets (and GET /api/bifrost/presets) never has to
// re-derive which lane an untagged preset resolved to, and bifrost.New's
// own user-match check never trips on a preset this function assigned.
func presetsForLane(presets []config.BifrostPreset, laneUser string, isFirstLane bool) []config.BifrostPreset {
	var out []config.BifrostPreset
	for _, p := range presets {
		switch {
		case p.User == laneUser:
			out = append(out, p)
		case p.User == "" && isFirstLane:
			resolved := p
			resolved.User = laneUser
			out = append(out, resolved)
		}
	}
	return out
}

// registerDashboardRoutes registers /dashboard, /bifrost, and their
// supporting /api/* and /static/* routes onto mux.
//
// Deliberately separate from registerTasksWebRoutesAt: that registrar is
// shared with `aida tasks web --standalone`, a 30-second ephemeral tasks
// browser that must not carry a remote-execution surface (bifrost) or a
// probe ticker (fleet). See TestDashboardRoutesAbsentFromStandalone.
func registerDashboardRoutes(mux *http.ServeMux, d dashboardDeps) {
	// Two guards, because pages and APIs face different threats.
	//
	// handle (strict) is for the API: full Origin / Sec-Fetch-Site / JSON
	// content-type checks, since those routes read state and trigger ssh.
	//
	// handlePage is for the HTML and static assets, and enforces ONLY the
	// loopback Host pin. Fetch metadata is deliberately not consulted there:
	// a top-level navigation carries Sec-Fetch-Site describing where the
	// user came FROM, so arriving from any other origin -- a link on another
	// local dev server (same-site), a bookmark in a doc page (cross-site) --
	// would fail the strict check and render a raw JSON error instead of the
	// page. That is a usability trap with no security benefit: serving HTML
	// and a JS bundle to a navigation is not a state change, and the page's
	// own fetches are same-origin so they still meet the strict bar. Found
	// by actually loading /dashboard in a browser from another port.
	handle := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, guardLocal(h))
	}
	handlePage := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, guardLocalHost(h))
	}

	dashboardHandler := htmlPageHandler(dashboardWebHTML)
	handlePage("GET /dashboard", dashboardHandler)
	handlePage("GET /dashboard/", dashboardHandler)

	bifrostHandler := htmlPageHandler(bifrostWebHTML)
	handlePage("GET /bifrost", bifrostHandler)
	handlePage("GET /bifrost/", bifrostHandler)

	arrowSub, err := fs.Sub(arrowAssetsFS, "assets/arrow")
	if err != nil {
		// Only reachable if the //go:embed directive above is broken --
		// a build-time invariant, not something that can happen at
		// runtime from user input.
		panic("dashboard: bad arrow asset embed: " + err.Error())
	}
	arrowFileServer := http.StripPrefix("/static/arrow/", http.FileServerFS(arrowSub))
	handlePage("GET /static/arrow/", func(w http.ResponseWriter, r *http.Request) {
		arrowFileServer.ServeHTTP(w, r)
	})

	handle("GET /api/fleet", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, d.Fleet.Snapshot(r.Context()))
	})
	handle("POST /api/fleet/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// Refresh() itself kicks off the probe round on its own
		// goroutine and returns immediately (see fleet.Cache.Refresh) --
		// this handler must not add any blocking of its own on top of
		// that, or a slow/sleeping device would make the button hang.
		d.Fleet.Refresh()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "refreshing": true})
	})

	flow := newFlowCache()
	handle("GET /api/dashboard/flow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, flow.get(parseFlowQuery(r)))
	})

	registerModelsRoutes(handle, d.ModelsPath, modelsProbeAdapter)

	registerBifrostRoutes(handle, d.Bifrost, d.BifrostLaneOrder)
}

// htmlPageHandler serves a static embedded HTML page, mirroring the
// /runs page's headers in tasks_web.go (no-store: this is a live
// dashboard, never a page a browser should cache).
func htmlPageHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	}
}

// guardLocal rejects requests a browser on a foreign origin could have
// produced. Loopback binding is NOT a boundary against a browser: any site
// the user visits can fetch() 127.0.0.1, and while same-origin policy stops
// it reading the reply, it does not stop the request executing. For routes
// that can type into a live Claude session on another machine, that matters.
//
// Four independent checks; any one failing rejects the request. No
// token/cookie CSRF scheme on top of this -- that needs bootstrap state
// for no gain on a daemon with no auth concept.
// guardLocalHost enforces only the loopback Host pin. It is the guard for
// page and static-asset routes.
//
// A top-level navigation's Sec-Fetch-Site describes where the user came
// FROM, so the strict guard below would reject arriving at /dashboard from
// a link on another local port (same-site) or a bookmark on a doc page
// (cross-site), rendering a raw JSON error instead of the page. Serving
// HTML and a JS bundle to a navigation is not a state change, and the
// page's own XHRs are same-origin and still meet the strict bar, so
// nothing is given up by allowing the navigation itself.
//
// The Host pin is retained because it is the check that actually stops DNS
// rebinding, which is a real threat to a page route: it is how an attacker
// would get same-origin script execution against the daemon in the first
// place.
func guardLocalHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hostIsLoopback(r.Host) {
			httpError(w, http.StatusForbidden, "host not permitted")
			return
		}
		next(w, r)
	}
}

func guardLocal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Host pinned. Any port is acceptable -- the daemon can run on
		// an alternate port -- but the host itself must be loopback. This
		// is what actually blocks DNS rebinding, which passes every
		// Origin/Sec-Fetch-Site check below (the attacker's DNS answer
		// controls what the browser considers "same-origin").
		if !hostIsLoopback(r.Host) {
			httpError(w, http.StatusForbidden, "host not permitted")
			return
		}

		// 2. Sec-Fetch-Site. Absent means an old browser or a non-fetch
		// client (curl); "none" means the user typed the URL or used a
		// bookmark; "same-origin" is the normal case. Anything else means
		// another site's page issued this request.
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
		default:
			httpError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}

		// 3. Origin, when present, must also resolve to loopback.
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !hostIsLoopback(u.Host) {
				httpError(w, http.StatusForbidden, "origin not permitted")
				return
			}
		}

		// 4. Mutating verbs require an explicit application/json
		// Content-Type. This forces the browser into a CORS preflight
		// (OPTIONS) that this server never answers with an
		// Access-Control-Allow-* header, which blocks the classic
		// simple-request form-POST bypass (a <form> or fetch() with
		// text/plain never triggers a preflight, so without this check a
		// foreign page could still fire the mutation blind).
		if isMutatingMethod(r.Method) {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}

		next(w, r)
	}
}

func isMutatingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// hostIsLoopback reports whether hostport (an http.Request.Host or a
// parsed URL's Host) names 127.0.0.1, localhost, or ::1 -- with or
// without a port, and with or without IPv6 brackets. Any port is
// accepted; only the host part is checked.
func hostIsLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else {
		// No port present (or a malformed hostport) -- fall back to
		// treating the whole string as the host, stripping IPv6
		// brackets by hand (net.SplitHostPort only strips them when a
		// port is also present).
		host = strings.Trim(hostport, "[]")
	}
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// ---- Bifrost handlers ----

const errNoHerdrDevice = "no herdr device configured (set herdr: true on a device in ~/.aida/config.yaml)"

// maxBifrostBodyBytes bounds POST bodies to /api/bifrost/* before JSON
// decoding. Every real payload (a pane-send string, a preset name) is
// tiny; this is a sanity cap, not a feature limit.
const maxBifrostBodyBytes = 64 * 1024

type bifrostStatusResponse struct {
	Running bool   `json:"running"`
	Version string `json:"version"`
	Socket  string `json:"socket"`
	Raw     string `json:"raw"`
	Error   string `json:"error"`
}

type bifrostAgentsResponse struct {
	Agents    []bifrost.Agent `json:"agents"`
	FetchedAt time.Time       `json:"fetched_at"`
	// HerdrOK and Error keep their pre-multi-lane meaning for backward
	// compatibility: "every configured lane answered" and a combined
	// message when one or more didn't. Lanes carries the per-lane detail
	// a caller that knows about lanes should actually use.
	HerdrOK bool                         `json:"herdr_ok"`
	Error   string                       `json:"error"`
	Lanes   map[string]bifrostLaneStatus `json:"lanes"`
}

// bifrostLaneStatus is one entry of bifrostAgentsResponse.Lanes: whether
// that lane's herdr call succeeded, and its error if not.
type bifrostLaneStatus struct {
	HerdrOK bool   `json:"herdr_ok"`
	Error   string `json:"error,omitempty"`
}

type bifrostReadResponse struct {
	Target string `json:"target"`
	Source string `json:"source"`
	Lines  int    `json:"lines"`
	Text   string `json:"text"`
	Error  string `json:"error,omitempty"`
}

type bifrostSendResponse struct {
	OK     bool   `json:"ok"`
	Target string `json:"target"`
	Bytes  int    `json:"bytes"`
	TookMs int64  `json:"took_ms"`
	Error  string `json:"error,omitempty"`
}

type bifrostPresetsResponse struct {
	Presets []bifrost.Preset `json:"presets"`
}

// selectBifrostLane resolves a ?lane=/"lane" value to a Client: empty
// selects laneOrder[0] (the default lane), and anything else must be an
// exact key of clients. ok is false for an unknown lane (including "no
// lanes configured at all," which laneOrder being empty already implies)
// -- callers map that to a 400, the same way every other bad-input path
// in this file does.
func selectBifrostLane(clients map[string]*bifrost.Client, laneOrder []string, lane string) (client *bifrost.Client, resolvedLane string, ok bool) {
	if lane == "" {
		if len(laneOrder) == 0 {
			return nil, "", false
		}
		lane = laneOrder[0]
	}
	c, found := clients[lane]
	if !found {
		return nil, lane, false
	}
	return c, lane, true
}

// registerBifrostRoutes wires the six /api/bifrost/* endpoints against a
// map of lane clients. When clients is empty (no device in config.yaml
// sets herdr: true), every route short-circuits to a 200 with a
// non-empty "error" -- a 404 here would make the /bifrost page look
// broken rather than simply unconfigured.
func registerBifrostRoutes(handle func(string, http.HandlerFunc), clients map[string]*bifrost.Client, laneOrder []string) {
	if len(clients) == 0 {
		unconfigured := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, map[string]any{
				"error":    errNoHerdrDevice,
				"herdr_ok": false,
			})
		}
		handle("GET /api/bifrost/status", unconfigured)
		handle("GET /api/bifrost/agents", unconfigured)
		handle("GET /api/bifrost/agents/{target}/read", unconfigured)
		handle("POST /api/bifrost/agents/{target}/send", unconfigured)
		handle("GET /api/bifrost/presets", unconfigured)
		handle("POST /api/bifrost/sessions", unconfigured)
		return
	}

	handle("GET /api/bifrost/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		client, lane, ok := selectBifrostLane(clients, laneOrder, r.URL.Query().Get("lane"))
		if !ok {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("unknown lane %q", lane))
			return
		}
		st, err := client.Status(r.Context())
		resp := bifrostStatusResponse{Running: st.Running, Version: st.Version, Socket: st.Socket, Raw: st.Raw}
		if err != nil {
			status, msg := classifyBifrostErr(err)
			if status != http.StatusOK {
				httpError(w, status, msg)
				return
			}
			resp.Error = msg
		}
		writeJSON(w, http.StatusOK, resp)
	})

	// GET /api/bifrost/agents fans out over EVERY configured lane
	// (unlike the other routes, it has no ?lane= -- the whole point of
	// the agent board is seeing every lane at once), tagging each
	// bifrost.Agent with the lane it came from and folding each lane's
	// outcome into Lanes. A single lane's failure (herdr down for that
	// user, a bad ssh hop, ...) must not blank out the other lanes'
	// agents, so every error here -- including one classifyBifrostErr
	// would otherwise treat as a 400/500 -- is recorded per-lane rather
	// than aborting the request.
	handle("GET /api/bifrost/agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		var allAgents []bifrost.Agent
		lanes := make(map[string]bifrostLaneStatus, len(laneOrder))
		allOK := true
		var errParts []string

		for _, lane := range laneOrder {
			client := clients[lane]
			agents, err := client.Agents(r.Context())
			if err != nil {
				_, msg := classifyBifrostErr(err)
				lanes[lane] = bifrostLaneStatus{HerdrOK: false, Error: msg}
				allOK = false
				errParts = append(errParts, lane+": "+msg)
				continue
			}
			lanes[lane] = bifrostLaneStatus{HerdrOK: true}
			for _, a := range agents {
				a.Lane = lane
				allAgents = append(allAgents, a)
			}
		}
		if allAgents == nil {
			allAgents = []bifrost.Agent{}
		}
		writeJSON(w, http.StatusOK, bifrostAgentsResponse{
			Agents:    allAgents,
			FetchedAt: time.Now(),
			HerdrOK:   allOK,
			Error:     strings.Join(errParts, "; "),
			Lanes:     lanes,
		})
	})

	handle("GET /api/bifrost/agents/{target}/read", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		client, lane, ok := selectBifrostLane(clients, laneOrder, r.URL.Query().Get("lane"))
		if !ok {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("unknown lane %q", lane))
			return
		}
		target := r.PathValue("target")
		source := r.URL.Query().Get("source")
		lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))

		text, err := client.Read(r.Context(), target, source, lines)
		resp := bifrostReadResponse{Target: target, Source: source, Lines: lines}
		if err != nil {
			status, msg := classifyBifrostErr(err)
			if status != http.StatusOK {
				httpError(w, status, msg)
				return
			}
			resp.Error = msg
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp.Text = text
		writeJSON(w, http.StatusOK, resp)
	})

	handle("POST /api/bifrost/agents/{target}/send", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		target := r.PathValue("target")

		r.Body = http.MaxBytesReader(w, r.Body, maxBifrostBodyBytes)
		var body struct {
			Text  string `json:"text"`
			Enter bool   `json:"enter"`
			Lane  string `json:"lane"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		client, lane, ok := selectBifrostLane(clients, laneOrder, body.Lane)
		if !ok {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("unknown lane %q", lane))
			return
		}

		start := time.Now()
		err := client.Send(r.Context(), target, body.Text, body.Enter)
		took := time.Since(start).Milliseconds()
		if err != nil {
			status, msg := classifyBifrostErr(err)
			if status != http.StatusOK {
				httpError(w, status, msg)
				return
			}
			writeJSON(w, http.StatusOK, bifrostSendResponse{Target: target, Error: msg})
			return
		}
		writeJSON(w, http.StatusOK, bifrostSendResponse{OK: true, Target: target, Bytes: len(body.Text), TookMs: took})
	})

	// GET /api/bifrost/presets spans every lane too (like /agents, and
	// for the same reason: the "Start session" control needs to offer
	// every preset regardless of which lane is currently selected
	// elsewhere on the page). Each bifrost.Preset already carries its
	// resolved User (see presetsForLane), so the caller can tell which
	// lane a preset belongs to and pass that back as "lane" on
	// POST /api/bifrost/sessions.
	handle("GET /api/bifrost/presets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var presets []bifrost.Preset
		for _, lane := range laneOrder {
			presets = append(presets, clients[lane].Presets...)
		}
		if presets == nil {
			presets = []bifrost.Preset{}
		}
		writeJSON(w, http.StatusOK, bifrostPresetsResponse{Presets: presets})
	})

	handle("POST /api/bifrost/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, maxBifrostBodyBytes)
		var body struct {
			Preset string `json:"preset"`
			Lane   string `json:"lane"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Preset == "" {
			httpError(w, http.StatusBadRequest, "missing preset")
			return
		}
		client, lane, ok := selectBifrostLane(clients, laneOrder, body.Lane)
		if !ok {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("unknown lane %q", lane))
			return
		}

		if err := client.StartPreset(r.Context(), body.Preset); err != nil {
			status, msg := classifyBifrostErr(err)
			if status != http.StatusOK {
				httpError(w, status, msg)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
}

// classifyBifrostErr maps a bifrost package error to the HTTP status the
// handler should use, and the message to surface.
//
//   - *bifrost.RemoteError (herdr/ssh ran but reported failure): 200 --
//     the caller folds msg into the response body's "error" field, because
//     one red banner on the page is far easier to render than distinguishing
//     five different 5xx causes.
//   - *bifrost.ValidationError (bad input caught before any remote call):
//     400.
//   - *bifrost.ParseError (herdr responded, but not in a shape we
//     understand -- our bug, not the user's): 500.
//   - anything else: 500.
func classifyBifrostErr(err error) (status int, msg string) {
	var remoteErr *bifrost.RemoteError
	var valErr *bifrost.ValidationError
	var parseErr *bifrost.ParseError
	switch {
	case errors.As(err, &remoteErr):
		return http.StatusOK, err.Error()
	case errors.As(err, &valErr):
		return http.StatusBadRequest, err.Error()
	case errors.As(err, &parseErr):
		return http.StatusInternalServerError, err.Error()
	default:
		return http.StatusInternalServerError, err.Error()
	}
}

// bifrostPresetsFromConfig converts config-layer preset declarations
// (read from config.yaml's bifrost.presets block) into internal/bifrost's
// Preset type. This is the "call site" conversion internal/config/bifrost.go
// mentions: config can't import bifrost, so whoever builds the
// bifrost.Client (serve.go) calls this first.
func bifrostPresetsFromConfig(presets []config.BifrostPreset) []bifrost.Preset {
	out := make([]bifrost.Preset, len(presets))
	for i, p := range presets {
		out[i] = bifrost.Preset{Name: p.Name, User: p.User, Cwd: p.Cwd, Argv: append([]string(nil), p.Argv...)}
	}
	return out
}

// ---- /api/dashboard/flow ----

const (
	flowDefaultWindow = "24h"
	flowDefaultLimit  = 200
	flowMaxLimit      = 1000
	flowCacheTTL      = 30 * time.Second
)

// flowResponse is the aggregated pipeline graph served to the dashboard's
// Flow panel: one node per phase (bare span name -- parse, route, execute,
// verify, synthesize, quality -- not "execute:sqlite", see TracingSpan's
// doc) and per source, plus edges walking consecutive spans within each
// run. Everything here is derived from real runs.Run records -- Measured
// is always true; synthesizing unmeasured topology is a later change.
type flowResponse struct {
	Nodes          []flowNode `json:"nodes"`
	Edges          []flowEdge `json:"edges"`
	Window         string     `json:"window"`
	RunCount       int        `json:"run_count"`
	Truncated      bool       `json:"truncated"`
	TotalCostUSD   float64    `json:"total_cost_usd"`
	TotalTokensIn  int        `json:"total_tokens_in"`
	TotalTokensOut int        `json:"total_tokens_out"`
	LLMCalls       int        `json:"llm_calls"`
	GeneratedAt    time.Time  `json:"generated_at"`
}

type flowNode struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"` // phase | source | sink
	Count    int    `json:"count"`
	P50Ms    int64  `json:"p50_ms"`
	Measured bool   `json:"measured"`
}

type flowEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Count    int    `json:"count"`
	Measured bool   `json:"measured"`
}

// flowCache holds the most recently computed flowResponse behind a 30s
// TTL, keyed by the (window, limit) query it was computed for. There are
// on the order of ~750 run files at ~30 KB each; re-reading and
// re-aggregating all of them on every 3s dashboard poll would be a real
// cost for a page that's just sitting open in a tab.
type flowCache struct {
	mu        sync.RWMutex
	key       string
	resp      flowResponse
	expiresAt time.Time
}

func newFlowCache() *flowCache {
	return &flowCache{}
}

func flowCacheKey(window string, limit int) string {
	return fmt.Sprintf("%s|%d", window, limit)
}

func (c *flowCache) get(window string, limit int) flowResponse {
	key := flowCacheKey(window, limit)

	c.mu.RLock()
	if c.key == key && time.Now().Before(c.expiresAt) {
		resp := c.resp
		c.mu.RUnlock()
		return resp
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under the write lock: another goroutine may have already
	// recomputed this exact key while we were waiting for it.
	if c.key == key && time.Now().Before(c.expiresAt) {
		return c.resp
	}
	resp := buildFlowResponse(window, limit)
	c.key = key
	c.resp = resp
	c.expiresAt = time.Now().Add(flowCacheTTL)
	return resp
}

// parseFlowQuery reads ?window= and ?limit= off the request, applying
// the documented defaults and bounds. Malformed values fall back to the
// default rather than erroring -- this is a dashboard widget, not an API
// contract worth 400ing over a typo'd query string.
func parseFlowQuery(r *http.Request) (window string, limit int) {
	window = strings.TrimSpace(r.URL.Query().Get("window"))
	if window == "" {
		window = flowDefaultWindow
	} else if _, err := time.ParseDuration(window); err != nil {
		window = flowDefaultWindow
	}

	limit = flowDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > flowMaxLimit {
		limit = flowMaxLimit
	}
	return window, limit
}

// buildFlowResponse does the actual work: list runs, filter to the
// window, cap at limit, and aggregate. A missing ~/.aida/runs/ directory
// is not an error -- runs.List returns (nil, nil) on ENOENT -- so this
// naturally produces an empty-but-200 response rather than 500ing.
func buildFlowResponse(windowStr string, limit int) flowResponse {
	windowDur, err := time.ParseDuration(windowStr)
	if err != nil {
		windowDur = 24 * time.Hour
		windowStr = flowDefaultWindow
	}
	now := time.Now()
	cutoff := now.Add(-windowDur)

	ids, _ := runs.List() // nil, nil on a missing runs dir -- handled below like any other empty result

	matched := make([]*runs.Run, 0, min(len(ids), limit+1))
	for _, id := range ids {
		run, lerr := runs.Load(id)
		if lerr != nil {
			continue // corrupt/unreadable run file -- skip it, don't fail the whole widget
		}
		if run.StartedAt.Before(cutoff) {
			// runs.List returns ids newest-first (timestamp-prefixed);
			// once we fall past the window cutoff nothing older matches
			// either.
			break
		}
		matched = append(matched, run)
	}

	truncated := false
	if len(matched) > limit {
		truncated = true
		matched = matched[:limit]
	}

	phaseAgg := map[string]*flowNodeAgg{}
	sourceAgg := map[string]*flowNodeAgg{}
	edgeAgg := map[[2]string]int{}

	var totalCost float64
	var totalIn, totalOut, llmCalls int

	for _, run := range matched {
		if run.Tracing != nil {
			totalCost += run.Tracing.TotalCost
			totalIn += run.Tracing.TotalIn
			totalOut += run.Tracing.TotalOut
			llmCalls += run.Tracing.LLMCalls

			for _, span := range run.Tracing.Spans {
				a := phaseAgg[span.Name]
				if a == nil {
					a = &flowNodeAgg{id: "phase:" + span.Name, label: span.Name, kind: "phase"}
					phaseAgg[span.Name] = a
				}
				a.count++
				a.durations = append(a.durations, span.DurationMs)
			}

			// Collapse consecutive identical span names before forming
			// edges: a run with a run of several back-to-back "execute"
			// spans (one per source) should contribute one node, not a
			// self-loop or a fan of duplicate edges.
			collapsed := collapseConsecutiveSpanNames(run.Tracing.Spans)
			for i := 0; i+1 < len(collapsed); i++ {
				key := [2]string{"phase:" + collapsed[i], "phase:" + collapsed[i+1]}
				edgeAgg[key]++
			}
		}

		for _, phase := range run.Phases {
			for _, src := range phase.Sources {
				a := sourceAgg[src.Name]
				if a == nil {
					a = &flowNodeAgg{id: "source:" + src.Name, label: src.Name, kind: "source"}
					sourceAgg[src.Name] = a
				}
				a.count++
				a.durations = append(a.durations, src.DurationMs)
			}
		}
	}

	nodes := make([]flowNode, 0, len(phaseAgg)+len(sourceAgg))
	for _, a := range phaseAgg {
		nodes = append(nodes, a.toNode())
	}
	for _, a := range sourceAgg {
		nodes = append(nodes, a.toNode())
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	edges := make([]flowEdge, 0, len(edgeAgg))
	for k, count := range edgeAgg {
		edges = append(edges, flowEdge{From: k[0], To: k[1], Count: count, Measured: true})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})

	return flowResponse{
		Nodes:          nodes,
		Edges:          edges,
		Window:         windowStr,
		RunCount:       len(matched),
		Truncated:      truncated,
		TotalCostUSD:   totalCost,
		TotalTokensIn:  totalIn,
		TotalTokensOut: totalOut,
		LLMCalls:       llmCalls,
		GeneratedAt:    now,
	}
}

// flowNodeAgg accumulates occurrence count + a duration sample for one
// node (a phase span name, or a source name) while walking runs.
type flowNodeAgg struct {
	id, label, kind string
	count           int
	durations       []int64
}

func (a *flowNodeAgg) toNode() flowNode {
	return flowNode{
		ID:       a.id,
		Label:    a.label,
		Kind:     a.kind,
		Count:    a.count,
		P50Ms:    p50Ms(a.durations),
		Measured: true,
	}
}

// p50Ms returns the median of durations, or 0 for an empty slice.
func p50Ms(durations []int64) int64 {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]int64(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[(len(sorted)-1)/2]
}

// collapseConsecutiveSpanNames returns the span name sequence with
// consecutive duplicates merged into one, e.g.
// [parse classify execute execute execute verify] -> [parse classify execute verify].
// Edges are built by walking this collapsed sequence, not the raw one.
func collapseConsecutiveSpanNames(spans []runs.TracingSpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		if len(out) > 0 && out[len(out)-1] == s.Name {
			continue
		}
		out = append(out, s.Name)
	}
	return out
}
