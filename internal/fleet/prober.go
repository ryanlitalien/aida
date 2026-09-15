package fleet

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/remotex"
)

// Prober knows how to answer two questions: "what does tailscale think of
// the fleet" (TailscaleStatus, one call, see tailscale.go) and "what's the
// full story on this one device" (ProbeOne, below). It holds no mutable
// state of its own -- Cache is what remembers results between calls.
type Prober struct {
	Runner remotex.Runner

	// Now is injectable for deterministic ProbedAt/ProbeTookMs in tests;
	// nil means time.Now.
	Now func() time.Time
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// firstOrEmpty returns ss[0], or "" if ss is empty. Split out mainly so
// nothing in ProbeOne indexes a possibly-empty slice directly -- ProbeOne
// must never panic (see its doc comment), and this is one of the few
// places a naive index would be tempting.
func firstOrEmpty(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// ProbeOne produces the current DeviceStatus for a single device. It never
// blocks longer than ctx allows and, per the package contract, never
// panics: any unexpected failure (a malformed response, a bad assumption
// about probe output) becomes DeviceStatus.Error, not a crash that would
// take the whole aida serve process down with it (this runs inside an HTTP
// handler's request path via Cache).
func (p *Prober) ProbeOne(ctx context.Context, d config.DeviceConfig, ts map[string]TSNode) (status DeviceStatus) {
	start := p.now()
	status = DeviceStatus{
		Name:  d.Name,
		Role:  d.Role,
		Probe: d.ProbeMode(),
		Self:  d.Self,
		Herdr: d.Herdr,
		Notes: d.Notes,
		Reach: ReachUnknown,
	}
	defer func() {
		if r := recover(); r != nil {
			status.Reach = ReachUnknown
			status.Aida = nil
			status.Error = fmt.Sprintf("fleet: probe panic: %v", r)
		}
		status.ProbedAt = p.now()
		status.ProbeTookMs = status.ProbedAt.Sub(start).Milliseconds()
	}()

	switch d.ProbeMode() {
	case config.ProbeLocal:
		// This device IS the process answering the dashboard request --
		// no network call could tell us anything a self-check can't.
		// Self is trusted, not probed: Healthz: true here means "this
		// code is running", not "we verified a healthz endpoint".
		status.Reach = ReachLocal
		status.Aida = &AidaStatus{Healthz: true}
		status.Hardware = p.probeOrStaticHardware(ctx, d, "", true)
		return status

	case config.ProbeNone:
		// Operator declared this device exists but opted it out of all
		// probing (e.g. a NAS or appliance with no aida agent). Static
		// card, no calls of any kind -- except a hand-entered
		// config.yaml hardware: override, if one is set.
		status.Reach = ReachUnknown
		status.Hardware = staticHardware(d)
		return status
	}

	// ssh and tailscale-only both start from tailscale's reachability
	// view; only ssh goes on to probe aida's healthz endpoint.
	node, found := resolveTSNode(d.TailscaleHost(), ts)
	if found {
		status.TailscaleIP = firstOrEmpty(node.TailscaleIPs)
		status.TailscaleOS = node.OS
		status.LastSeen = node.LastSeen
		if node.Online {
			status.Reach = ReachOnline
		} else {
			status.Reach = ReachOffline
		}
	}
	// !found leaves status.Reach at its ReachUnknown zero value: the
	// device isn't in tailscale's peer/self map at all, which is neither
	// "known online" nor "known offline".

	if d.ProbeMode() == config.ProbeTailscaleOnly {
		// No ssh transport is ever attempted for tailscale-only, so a
		// live hardware probe is impossible -- e.g. beast, a Windows
		// host aida never ssh's into. Fall back to a hand-entered
		// config.yaml hardware: override, if one is set.
		status.Hardware = staticHardware(d)
		return status
	}

	// probe: ssh from here down.
	if found && !node.Online {
		// Skip the ssh probe entirely when tailscale affirmatively says
		// the node is down: there's no point burning a several-second
		// ConnectTimeout dialing a sleeping laptop. photon in particular
		// is genuinely asleep most of the time -- without this skip,
		// every fleet poll would pay photon's full ssh timeout for
		// nothing. We only skip on a KNOWN-offline signal, not on
		// !found (unknown), since an unknown device might still be
		// reachable directly (tailscale down, LAN/hosts-file reachable,
		// etc) and it costs nothing extra to find out.
		return status
	}

	target := d.SSHTarget()
	if err := remotex.ValidateHost(target); err != nil {
		status.Error = err.Error()
		return status
	}

	res, err := remotex.RunRemote(ctx, p.Runner, target, "", []string{
		"curl", "-fsS", "--max-time", "3",
		fmt.Sprintf("http://127.0.0.1:%d/healthz", d.Port()),
	}, probeCallTimeout(ctx))
	if res.TimedOut {
		status.Error = "ssh healthz probe timed out"
		return status
	}
	if res.ExitCode == 0 && strings.Contains(string(res.Stdout), "ok") {
		status.Aida = &AidaStatus{Healthz: true}
		status.Hardware = p.probeOrStaticHardware(ctx, d, target, false)
		return status
	}

	// Interpret the exit code BEFORE err. A non-zero exit is an outcome,
	// not a transport failure -- execx.Run returns a fully populated Result
	// alongside an *exec.ExitError, so checking err first would collapse
	// every distinguishable outcome into the string "exit status N".
	switch res.ExitCode {
	case curlExitCouldNotConnect:
		// Nothing listening on the port. For this fleet that is the NORMAL
		// state of most hosts rather than a fault: only some machines run
		// `aida serve`, and photon runs `aida jarvis daemon`, which starts
		// no HTTP server at all. Saying "exit status 7" paints the grid red
		// for a healthy fleet. Aida stays nil -- "we don't know" -- rather
		// than false, which would claim we found a broken daemon.
		//
		// ssh itself DID work here (that's how we got a curl exit code
		// back at all), so it's still worth the one extra round trip for
		// hardware info even though aida isn't running.
		status.Error = fmt.Sprintf("no aida daemon on :%d", d.Port())
		status.Hardware = p.probeOrStaticHardware(ctx, d, target, false)
		return status
	case sshExitConnectionFailed:
		// ssh itself could not reach the host (refused, timed out, key
		// rejected). Distinct from "the host is up but aida isn't" -- and
		// with no working ssh transport, a hardware probe would just fail
		// too, so skip it rather than pay for a second doomed round trip.
		status.Error = "ssh unreachable"
		return status
	}

	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Error = fmt.Sprintf("healthz probe failed (exit %d): %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	// ssh transport worked (we got a well-formed, if unexpected, curl
	// exit code back); still worth trying for hardware info.
	status.Hardware = p.probeOrStaticHardware(ctx, d, target, false)
	return status
}

// Exit codes worth telling apart, so the dashboard can say what actually
// happened instead of surfacing a bare number.
const (
	// curl(1): failed to connect to host.
	curlExitCouldNotConnect = 7
	// ssh(1) reserves 255 for its own connection errors, as opposed to
	// passing through the remote command's status.
	sshExitConnectionFailed = 255
)
