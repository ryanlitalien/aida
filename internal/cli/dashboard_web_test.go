package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/bifrost"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/fleet"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/remotex"
	"github.com/ryanlitalien/aida/internal/runs"
)

// ---- test helpers ----

// newTestFleetCache builds a fleet.Cache with no configured devices, so
// tests that don't care about fleet probing never touch the network (the
// FakeRunner would refuse to shell out anyway, but an empty device list
// keeps doRefresh a no-op).
func newTestFleetCache(fr *remotex.FakeRunner) *fleet.Cache {
	p := &fleet.Prober{Runner: fr}
	return fleet.NewCache(p, nil, config.FleetConfig{})
}

// newTestBifrostClient builds a bifrost.Client backed by a FakeRunner --
// no test in this file may spawn a real ssh process.
func newTestBifrostClient(t *testing.T, fr *remotex.FakeRunner) *bifrost.Client {
	t.Helper()
	c, err := bifrost.New(fr, "minty", "heimdall", []bifrost.Preset{
		{Name: "claude-work", Cwd: "/home/heimdall/proj", Argv: []string{"claude", "--dangerously-skip-permissions"}},
	})
	if err != nil {
		t.Fatalf("bifrost.New: %v", err)
	}
	return c
}

// newTestDeps builds a minimal dashboardDeps with a single "heimdall"
// bifrost lane. bifrostClient may be nil to exercise the "no herdr
// device configured" path (an empty Bifrost map).
func newTestDeps(bifrostClient *bifrost.Client) dashboardDeps {
	deps := dashboardDeps{
		Cfg:     &config.Config{},
		Profile: "work",
		Fleet:   newTestFleetCache(&remotex.FakeRunner{}),
		Bifrost: map[string]*bifrost.Client{},
		Machine: "test-machine",
		Started: time.Now(),
	}
	if bifrostClient != nil {
		deps.Bifrost = map[string]*bifrost.Client{"heimdall": bifrostClient}
		deps.BifrostLaneOrder = []string{"heimdall"}
	}
	return deps
}

// newTestBrainAndJobs opens a scratch brain + jobs store under a fresh
// HOME, mirroring the fixture pattern in tasks_web_test.go.
func newTestBrainAndJobs(t *testing.T) (*brain.Brain, *jobs.Store) {
	t.Helper()
	store, err := jobs.Open("work")
	if err != nil {
		t.Fatalf("jobs.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	b, err := brain.Open(filepath.Join(t.TempDir(), "brain"), "work", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("brain.Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b, store
}

// ---- page + static asset serving ----

func TestDashboardPageServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/dashboard", "/dashboard/"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s Content-Type = %q, want text/html", path, ct)
		}
	}
}

func TestBifrostPageServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/bifrost", "/bifrost/"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s Content-Type = %q, want text/html", path, ct)
		}
	}
}

// TestArrowAssetServes is the one that catches a silently blank
// dashboard/bifrost page: if /static/arrow/index.mjs doesn't come back
// with a JS-ish Content-Type, the browser refuses to load it as an ES
// module and the page renders nothing with no visible error.
func TestArrowAssetServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/static/arrow/index.mjs")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want it to contain javascript", ct)
	}
}

// TestArrowChunkServes proves the relative import from index.mjs into
// chunks/ actually resolves under the mounted prefix.
func TestArrowChunkServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/static/arrow/chunks/internal-DchK7S7v.mjs")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// ---- registrar separation ----

func TestDashboardRoutesAbsentFromStandalone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b, store := newTestBrainAndJobs(t)

	mux := http.NewServeMux()
	registerTasksWebRoutes(mux, b, store, "work", false)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/dashboard", "/bifrost", "/api/fleet", "/api/models"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 (standalone tasks web must not carry dashboard routes)", path, resp.StatusCode)
		}
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/bifrost/agents/x/send", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	// Not a live route in the standalone mux, so it must not be 200 --
	// but the exact rejection code is 405, not 404: registerTasksWebRoutes
	// registers a catch-all "GET /" pattern for the standalone case
	// (htmlPath == ""), and Go 1.22's http.ServeMux resolves METHOD
	// mismatches against a path-matching pattern before falling through to
	// "no pattern matches at all" -- since "GET /" matches every path,
	// POST to any unregistered path here comes back 405, not 404. Verified
	// against the stdlib directly; not something this registrar controls.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /api/bifrost/agents/x/send status = %d, want non-200 (route must not be live in standalone mode)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/bifrost/agents/x/send status = %d, want 404 or 405", resp.StatusCode)
	}
}

func TestMuxRegistrationDoesNotPanic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b, store := newTestBrainAndJobs(t)

	mux := http.NewServeMux()
	registerTasksWebRoutes(mux, b, store, "work", false)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering dashboard routes alongside tasks routes panicked (duplicate ServeMux pattern): %v", r)
		}
	}()
	registerDashboardRoutes(mux, newTestDeps(nil))
}

// ---- guardLocal ----

func TestGuardLocalRejects(t *testing.T) {
	handler := guardLocal(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name        string
		method      string
		target      string
		secFetch    string
		origin      string
		contentType string
		want        int
	}{
		{
			name:   "foreign host rejected",
			method: http.MethodGet,
			target: "http://evil.com/x",
			want:   http.StatusForbidden,
		},
		{
			name:     "cross-site Sec-Fetch-Site rejected",
			method:   http.MethodGet,
			target:   "http://127.0.0.1:9999/x",
			secFetch: "cross-site",
			want:     http.StatusForbidden,
		},
		{
			name:     "same-site Sec-Fetch-Site rejected",
			method:   http.MethodGet,
			target:   "http://127.0.0.1:9999/x",
			secFetch: "same-site",
			want:     http.StatusForbidden,
		},
		{
			name:   "foreign Origin rejected",
			method: http.MethodGet,
			target: "http://127.0.0.1:9999/x",
			origin: "https://evil.com",
			want:   http.StatusForbidden,
		},
		{
			name:        "mutating verb with wrong Content-Type rejected",
			method:      http.MethodPost,
			target:      "http://127.0.0.1:9999/x",
			contentType: "text/plain",
			want:        http.StatusUnsupportedMediaType,
		},
		{
			name:        "happy path: no origin, JSON content type",
			method:      http.MethodPost,
			target:      "http://127.0.0.1:9999/x",
			contentType: "application/json",
			want:        http.StatusOK,
		},
		{
			name:   "happy path GET, no headers at all",
			method: http.MethodGet,
			target: "http://127.0.0.1:9999/x",
			want:   http.StatusOK,
		},
		{
			name:   "non-1610 loopback port still accepted",
			method: http.MethodGet,
			target: "http://127.0.0.1:54321/x",
			want:   http.StatusOK,
		},
		{
			name:   "localhost host accepted",
			method: http.MethodGet,
			target: "http://localhost:1610/x",
			want:   http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			if tt.secFetch != "" {
				req.Header.Set("Sec-Fetch-Site", tt.secFetch)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// ---- /api/fleet ----

func TestFleetEndpointColdCacheIsFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fr := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{ExitCode: 0, Stdout: []byte("ok")}, Delay: 5 * time.Second},
	}}
	p := &fleet.Prober{Runner: fr}
	devices := []config.DeviceConfig{{Name: "dev-a", Probe: config.ProbeSSH}}
	cache := fleet.NewCache(p, devices, config.FleetConfig{ProbeTimeoutSeconds: 10})

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, dashboardDeps{Fleet: cache})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	start := time.Now()
	resp, err := http.Get(srv.URL + "/api/fleet")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GET /api/fleet: %v", err)
	}
	defer resp.Body.Close()
	if elapsed > 100*time.Millisecond {
		t.Errorf("took %v on a cold cache, want <100ms", elapsed)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if refreshing, _ := got["refreshing"].(bool); !refreshing {
		t.Errorf("refreshing = %v, want true", got["refreshing"])
	}
}

// TestFleetEndpointIncludesHardware proves the hardware overlay (CPU/mem/
// GPU/disk) round-trips all the way through /api/fleet's JSON, not just
// through fleet.Prober in isolation: a probe: local device's Hardware
// field, once a probe round actually completes, must show up under
// devices[].hardware in the HTTP response the dashboard's JS reads.
func TestFleetEndpointIncludesHardware(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const macHW = "CPU=Apple M4 Pro\nMEM=24GB\nDISK=44G free of 926G\nGPU=Apple M4 Pro\n"
	fr := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match: func(name string, args []string, _ []byte) bool {
				return name == "sh" && len(args) == 2 && args[0] == "-c"
			},
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(macHW)},
		},
	}}
	p := &fleet.Prober{Runner: fr}
	devices := []config.DeviceConfig{{Name: "edith", Probe: config.ProbeLocal, Self: true}}
	cache := fleet.NewCache(p, devices, config.FleetConfig{ProbeTimeoutSeconds: 10})

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, dashboardDeps{Fleet: cache})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The first hit is a cold cache that kicks an async refresh (see
	// TestFleetEndpointColdCacheIsFast) -- poll until that round lands.
	deadline := time.Now().Add(2 * time.Second)
	var got map[string]any
	for {
		resp, err := http.Get(srv.URL + "/api/fleet")
		if err != nil {
			t.Fatalf("GET /api/fleet: %v", err)
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close()
		if refreshing, _ := got["refreshing"].(bool); !refreshing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fleet snapshot never finished refreshing")
		}
		time.Sleep(10 * time.Millisecond)
	}

	devicesOut, _ := got["devices"].([]any)
	if len(devicesOut) != 1 {
		t.Fatalf("devices = %v, want 1 entry", devicesOut)
	}
	dev, ok := devicesOut[0].(map[string]any)
	if !ok {
		t.Fatalf("devices[0] not an object: %v", devicesOut[0])
	}
	hw, ok := dev["hardware"].(map[string]any)
	if !ok {
		t.Fatalf("devices[0].hardware missing or wrong shape: %v", dev["hardware"])
	}
	for field, want := range map[string]string{
		"cpu":  "Apple M4 Pro",
		"mem":  "24GB",
		"disk": "44G free of 926G",
		"gpu":  "Apple M4 Pro",
	} {
		if got := hw[field]; got != want {
			t.Errorf("hardware.%s = %v, want %q", field, got, want)
		}
	}
}

// TestFleetEndpointTotalsExcludeWSLSubNodes is the critical no-double-
// counting regression at the HTTP-endpoint level: beast (a static
// hardware override, since it's a Windows host aida can't live-probe)
// and beast-wsl (a live ssh probe of the SAME physical machine's Linux
// side, reporting the SAME CPU and RTX 4070) must contribute exactly one
// machine's worth of cores/GPU to /api/fleet's totals, not two.
func TestFleetEndpointTotalsExcludeWSLSubNodes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	healthzScript, err := remotex.BuildRemoteScript([]string{
		"curl", "-fsS", "--max-time", "3", "http://127.0.0.1:1610/healthz",
	})
	if err != nil {
		t.Fatalf("BuildRemoteScript: %v", err)
	}
	const beastWSLHW = "CPU=12th Gen Intel(R) Core(TM) i7-12700F\nCORES=8\nMEM=9.7Gi\nDISK=767G free of 1007G\nGPU=NVIDIA GeForce RTX 4070, 12282 MiB\n"

	fr := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match:  func(name string, _ []string, _ []byte) bool { return name == "tailscale" },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(`{"Self":null,"Peer":{}}`)},
		},
		{
			Match: func(name string, _ []string, stdin []byte) bool {
				return name == "ssh" && bytes.Equal(stdin, healthzScript)
			},
			Result: remotex.Result{ExitCode: 0, Stdout: []byte("ok\n")},
		},
		{
			// Catch-all: beast-wsl's only other ssh call is the combined
			// hardware probe (its exact script text isn't reachable from
			// this package, so match on transport alone).
			Match:  func(name string, _ []string, _ []byte) bool { return name == "ssh" },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(beastWSLHW)},
		},
	}}
	p := &fleet.Prober{Runner: fr}
	devices := []config.DeviceConfig{
		{
			Name: "beast", Probe: config.ProbeTailscaleOnly,
			Hardware: &config.HardwareConfig{
				CPU: "Intel Core i7-12700F (18 threads)", Mem: "16GB",
				GPU: "NVIDIA GeForce RTX 4070 (12 GB)", Disk: "930GB", Cores: 18,
			},
		},
		{Name: "beast-wsl", Probe: config.ProbeSSH},
	}
	cache := fleet.NewCache(p, devices, config.FleetConfig{ProbeTimeoutSeconds: 10})

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, dashboardDeps{Fleet: cache})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	deadline := time.Now().Add(2 * time.Second)
	var got map[string]any
	for {
		resp, err := http.Get(srv.URL + "/api/fleet")
		if err != nil {
			t.Fatalf("GET /api/fleet: %v", err)
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close()
		if refreshing, _ := got["refreshing"].(bool); !refreshing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fleet snapshot never finished refreshing")
		}
		time.Sleep(10 * time.Millisecond)
	}

	totals, ok := got["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals missing or wrong shape: %v", got["totals"])
	}
	if machines, _ := totals["machines"].(float64); machines != 1 {
		t.Errorf("totals.machines = %v, want 1 (beast-wsl excluded as beast's own sub-node)", totals["machines"])
	}
	if cores, _ := totals["cores"].(float64); cores != 18 {
		t.Errorf("totals.cores = %v, want 18 (only beast's static value, not +8 from beast-wsl)", totals["cores"])
	}
	gpuGB, _ := totals["gpu_gb"].(float64)
	if gpuGB < 11.9 || gpuGB > 12.1 {
		t.Errorf("totals.gpu_gb = %v, want ~12 (a single RTX 4070's worth, not doubled)", gpuGB)
	}
}

// ---- bifrost handlers ----

func TestBifrostNilClientReturns200WithError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	paths := []string{"/api/bifrost/status", "/api/bifrost/agents", "/api/bifrost/presets"}
	for _, p := range paths {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", p, resp.StatusCode)
		}
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode %s: %v", p, err)
		}
		resp.Body.Close()
		errMsg, _ := got["error"].(string)
		if errMsg == "" {
			t.Errorf("%s: expected non-empty error, got %v", p, got)
		}
		if herdrOK, ok := got["herdr_ok"].(bool); !ok || herdrOK {
			t.Errorf("%s: herdr_ok = %v, want false", p, got["herdr_ok"])
		}
	}
}

func TestBifrostSendValidatesTarget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fr := &remotex.FakeRunner{}
	client := newTestBifrostClient(t, fr)

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(client))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/bifrost/agents/..%2F..%2Fetc/send", strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if got := fr.CallCount(); got != 0 {
		t.Errorf("FakeRunner recorded %d calls, want 0 (bad target must be rejected before any remote call)", got)
	}
}

func TestBifrostSendRequiresJSONContentType(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fr := &remotex.FakeRunner{}
	client := newTestBifrostClient(t, fr)

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(client))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/bifrost/agents/phone/send", strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
	if got := fr.CallCount(); got != 0 {
		t.Errorf("FakeRunner recorded %d calls, want 0", got)
	}
}

// ---- bifrost multi-lane fan-out ----

// twoLaneDeps builds a dashboardDeps with two bifrost lanes, "heimdall"
// (first/default) and "ryan", each backed by its own FakeRunner so the
// test can make one lane succeed and the other fail independently.
func twoLaneDeps(heimdallFR, ryanFR *remotex.FakeRunner) dashboardDeps {
	heimdallClient, err := bifrost.New(heimdallFR, "minty", "heimdall", []bifrost.Preset{
		{Name: "work-session", User: "heimdall", Cwd: "/home/heimdall/work", Argv: []string{"claude"}},
	})
	if err != nil {
		panic(err) // test-fixture construction only, never real config
	}
	ryanClient, err := bifrost.New(ryanFR, "minty", "ryan", []bifrost.Preset{
		{Name: "scarlett-978", User: "ryan", Cwd: "/home/ryan/dev/acme_widgets", Argv: []string{"claude"}},
	})
	if err != nil {
		panic(err)
	}
	return dashboardDeps{
		Cfg:     &config.Config{},
		Profile: "work",
		Fleet:   newTestFleetCache(&remotex.FakeRunner{}),
		Bifrost: map[string]*bifrost.Client{
			"heimdall": heimdallClient,
			"ryan":     ryanClient,
		},
		BifrostLaneOrder: []string{"heimdall", "ryan"},
		Machine:          "test-machine",
		Started:          time.Now(),
	}
}

func TestBifrostAgentsFansOutOverEveryLane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	heimdallAgents := `{"id":"1","result":{"agents":[{"agent":"claude","agent_status":"idle","name":"phone"}],"type":"agent_list"}}`
	heimdallFR := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{ExitCode: 0, Stdout: []byte(heimdallAgents)}},
	}}
	ryanFR := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{ExitCode: 1, Stderr: []byte("herdr: connection refused")}},
	}}

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, twoLaneDeps(heimdallFR, ryanFR))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/bifrost/agents")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a single lane's failure must not fail the whole request)", resp.StatusCode)
	}

	var got bifrostAgentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1 (only the heimdall lane returned an agent), got %+v", len(got.Agents), got.Agents)
	}
	if got.Agents[0].Name != "phone" {
		t.Errorf("Agents[0].Name = %q, want phone", got.Agents[0].Name)
	}
	if got.Agents[0].Lane != "heimdall" {
		t.Errorf("Agents[0].Lane = %q, want heimdall (the lane tag must be set by the fan-out, not herdr's wire)", got.Agents[0].Lane)
	}

	if got.Lanes == nil {
		t.Fatal("Lanes map is nil, want a per-lane status entry for each configured lane")
	}
	heimdallStatus, ok := got.Lanes["heimdall"]
	if !ok || !heimdallStatus.HerdrOK {
		t.Errorf("Lanes[heimdall] = %+v (ok=%v), want herdr_ok=true", heimdallStatus, ok)
	}
	ryanStatus, ok := got.Lanes["ryan"]
	if !ok || ryanStatus.HerdrOK {
		t.Errorf("Lanes[ryan] = %+v (ok=%v), want herdr_ok=false", ryanStatus, ok)
	}
	if ryanStatus.Error == "" {
		t.Error("Lanes[ryan].Error is empty, want the ryan lane's failure message")
	}

	// Backward compatibility: HerdrOK/Error at the top level still mean
	// "every lane ok" / a combined message, for any consumer that
	// predates multi-lane support.
	if got.HerdrOK {
		t.Error("top-level HerdrOK = true, want false (the ryan lane failed)")
	}
	if got.Error == "" {
		t.Error("top-level Error is empty, want a non-empty combined message")
	}
}

func TestBifrostUnknownLaneReturns400(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, twoLaneDeps(&remotex.FakeRunner{}, &remotex.FakeRunner{}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	getPaths := []string{
		"/api/bifrost/status?lane=nobody",
		"/api/bifrost/agents/phone/read?lane=nobody",
	}
	for _, p := range getPaths {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", p, resp.StatusCode)
		}
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/bifrost/agents/phone/send", strings.NewReader(`{"text":"hi","lane":"nobody"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST send status = %d, want 400", resp.StatusCode)
	}
}

func TestBifrostStatusDefaultsFirstLane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	heimdallFR := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{ExitCode: 0, Stdout: []byte("server:\n  status: running\n  version: 0.7.4\n")}},
	}}
	ryanFR := &remotex.FakeRunner{}

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, twoLaneDeps(heimdallFR, ryanFR))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No ?lane= at all -- must resolve to the first configured lane
	// (heimdall), not error, and must not touch the ryan lane's runner.
	resp, err := http.Get(srv.URL + "/api/bifrost/status")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got bifrostStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Running {
		t.Errorf("Running = false, want true (from the default/first lane)")
	}
	if got := ryanFR.CallCount(); got != 0 {
		t.Errorf("ryan lane FakeRunner CallCount = %d, want 0 (default lane must be heimdall, not ryan)", got)
	}
}

// ---- /api/dashboard/flow ----

func TestFlowEndpointEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/dashboard/flow")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (missing runs dir must not 500)", resp.StatusCode)
	}
	var got flowResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Nodes) != 0 {
		t.Errorf("Nodes = %v, want empty", got.Nodes)
	}
	if len(got.Edges) != 0 {
		t.Errorf("Edges = %v, want empty", got.Edges)
	}
	if got.RunCount != 0 {
		t.Errorf("RunCount = %d, want 0", got.RunCount)
	}
}

func TestFlowEndpointAggregates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	now := time.Now().UTC()
	mkRun := func(id string, startedAt time.Time) {
		r := &runs.Run{
			ID:        id,
			StartedAt: startedAt,
			Tracing: &runs.TracingData{
				Spans: []runs.TracingSpan{
					{Name: "parse", DurationMs: 100},
					{Name: "route", DurationMs: 10},
					{Name: "execute", DurationMs: 200},
					{Name: "execute", DurationMs: 220},
					{Name: "synthesize", DurationMs: 300},
				},
				TotalCost: 0.01,
				TotalIn:   100,
				TotalOut:  50,
				LLMCalls:  3,
			},
			Phases: []runs.PhaseRun{
				{Name: "execute", Sources: []runs.SourceRun{
					{Name: "snowflake", DurationMs: 150},
				}},
			},
		}
		if _, err := runs.Save(r); err != nil {
			t.Fatalf("runs.Save: %v", err)
		}
	}

	mkRun("20260101-000000-alpha", now.Add(-1*time.Hour))
	mkRun("20260101-000100-beta", now.Add(-2*time.Hour))

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, newTestDeps(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/dashboard/flow?window=24h&limit=200")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got flowResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.RunCount != 2 {
		t.Errorf("RunCount = %d, want 2", got.RunCount)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false")
	}

	nodeByID := map[string]flowNode{}
	for _, n := range got.Nodes {
		nodeByID[n.ID] = n
	}

	execNode, ok := nodeByID["phase:execute"]
	if !ok {
		t.Fatalf("missing phase:execute node, got %+v", got.Nodes)
	}
	if execNode.Count != 4 {
		t.Errorf("phase:execute count = %d, want 4 (2 spans x 2 runs)", execNode.Count)
	}
	if execNode.Kind != "phase" {
		t.Errorf("phase:execute kind = %q, want phase", execNode.Kind)
	}
	if !execNode.Measured {
		t.Error("phase:execute Measured = false, want true")
	}

	srcNode, ok := nodeByID["source:snowflake"]
	if !ok {
		t.Fatalf("missing source:snowflake node, got %+v", got.Nodes)
	}
	if srcNode.Count != 2 {
		t.Errorf("source:snowflake count = %d, want 2", srcNode.Count)
	}
	if srcNode.Kind != "source" {
		t.Errorf("source:snowflake kind = %q, want source", srcNode.Kind)
	}

	edgeByPair := map[[2]string]flowEdge{}
	for _, e := range got.Edges {
		edgeByPair[[2]string{e.From, e.To}] = e
	}
	// The double "execute" span collapses to one node in the edge walk,
	// so each run contributes exactly 3 edges: parse->route,
	// route->execute, execute->synthesize -- never execute->execute.
	wantEdges := [][2]string{
		{"phase:parse", "phase:route"},
		{"phase:route", "phase:execute"},
		{"phase:execute", "phase:synthesize"},
	}
	for _, pair := range wantEdges {
		e, ok := edgeByPair[pair]
		if !ok {
			t.Errorf("missing edge %v, got %+v", pair, got.Edges)
			continue
		}
		if e.Count != 2 {
			t.Errorf("edge %v count = %d, want 2", pair, e.Count)
		}
	}
	if _, ok := edgeByPair[[2]string{"phase:execute", "phase:execute"}]; ok {
		t.Error("found a self-loop execute->execute edge; consecutive duplicates should collapse")
	}

	if got.TotalCostUSD < 0.0199 || got.TotalCostUSD > 0.0201 {
		t.Errorf("TotalCostUSD = %v, want ~0.02", got.TotalCostUSD)
	}
	if got.TotalTokensIn != 200 {
		t.Errorf("TotalTokensIn = %d, want 200", got.TotalTokensIn)
	}
	if got.TotalTokensOut != 100 {
		t.Errorf("TotalTokensOut = %d, want 100", got.TotalTokensOut)
	}
	if got.LLMCalls != 6 {
		t.Errorf("LLMCalls = %d, want 6", got.LLMCalls)
	}
}

// ---- config <-> bifrost preset conversion ----

func TestBifrostPresetsFromConfig(t *testing.T) {
	in := []config.BifrostPreset{
		{Name: "claude-work", User: "ryan", Cwd: "/home/heimdall/proj", Argv: []string{"claude", "--dangerously-skip-permissions"}},
	}
	got := bifrostPresetsFromConfig(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Name != "claude-work" || got[0].User != "ryan" || got[0].Cwd != "/home/heimdall/proj" || len(got[0].Argv) != 2 {
		t.Errorf("converted preset = %+v", got[0])
	}
}

func TestPresetsForLane(t *testing.T) {
	presets := []config.BifrostPreset{
		{Name: "work-session", Cwd: "/home/heimdall/work", Argv: []string{"claude"}},          // untagged -> first lane only
		{Name: "scarlett-978", User: "ryan", Cwd: "/home/ryan/dev", Argv: []string{"claude"}}, // explicit lane
	}

	heimdall := presetsForLane(presets, "heimdall", true)
	if len(heimdall) != 1 || heimdall[0].Name != "work-session" || heimdall[0].User != "heimdall" {
		t.Errorf("presetsForLane(heimdall, isFirst=true) = %+v, want [work-session with resolved User=heimdall]", heimdall)
	}

	ryan := presetsForLane(presets, "ryan", false)
	if len(ryan) != 1 || ryan[0].Name != "scarlett-978" || ryan[0].User != "ryan" {
		t.Errorf("presetsForLane(ryan, isFirst=false) = %+v, want [scarlett-978]", ryan)
	}

	// An untagged preset must NOT leak into a non-first lane.
	notFirst := presetsForLane(presets, "someone-else", false)
	for _, p := range notFirst {
		if p.Name == "work-session" {
			t.Errorf("untagged preset %q leaked into non-first lane %q", p.Name, "someone-else")
		}
	}
}
