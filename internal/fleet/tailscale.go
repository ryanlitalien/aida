package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TSNode is the subset of a `tailscale status --json` node (Self or a
// Peer entry) that the dashboard cares about. We hand-roll this instead of
// importing tailscale.com/ipn/ipnstate to avoid pulling that whole module
// (and its dependency tree) into aida for four fields.
type TSNode struct {
	HostName     string
	DNSName      string
	TailscaleIPs []string
	Online       bool
	OS           string
	LastSeen     string
}

// tsStatusDoc mirrors just enough of `tailscale status --json`'s top level
// to unmarshal Self and Peer. Peer's real shape is keyed by node key
// ("nodekey:..."); we don't care about the keys themselves, only the
// node values, so we decode into a map and then flatten it in
// TailscaleStatus.
type tsStatusDoc struct {
	Self *TSNode
	Peer map[string]TSNode
}

// TailscaleStatus runs `tailscale status --json` ONCE for the whole fleet
// (never per host -- that's the whole point of doing it here rather than
// inside ProbeOne) and returns every node (self + peers) keyed by its
// HostName exactly as tailscale reports it. Matching a specific device to
// a node (case-insensitive, DNSName-label fallback) is resolveTSNode's job,
// not this function's -- keying by raw HostName here keeps this function a
// dumb, faithful parse of tailscale's own output.
//
// A missing/erroring tailscale binary is reported via the returned error,
// never as a panic: this call is never allowed to be fatal to the fleet
// dashboard. Callers should set Snapshot.TailscaleOK = (err == nil) and
// still produce a card (Reach: ReachUnknown) for every device.
func (p *Prober) TailscaleStatus(ctx context.Context) (map[string]TSNode, error) {
	res, err := p.Runner.Run(ctx, "tailscale", []string{"status", "--json"}, nil, probeCallTimeout(ctx))
	if err != nil {
		return nil, fmt.Errorf("fleet: tailscale status: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("fleet: tailscale status exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var doc tsStatusDoc
	if err := json.Unmarshal(res.Stdout, &doc); err != nil {
		return nil, fmt.Errorf("fleet: parse tailscale status: %w", err)
	}

	nodes := make(map[string]TSNode, len(doc.Peer)+1)
	if doc.Self != nil {
		nodes[doc.Self.HostName] = *doc.Self
	}
	for _, peer := range doc.Peer {
		nodes[peer.HostName] = peer
	}
	return nodes, nil
}

// tsFirstDNSLabel returns the leftmost label of a MagicDNS name, e.g.
// "minty.tail1a2b3c.ts.net." -> "minty". Used as the fallback match key
// when a device's declared tailscale_name doesn't equal any node's
// HostName verbatim (case aside).
func tsFirstDNSLabel(dnsName string) string {
	dnsName = strings.TrimSuffix(dnsName, ".")
	if i := strings.IndexByte(dnsName, '.'); i >= 0 {
		return dnsName[:i]
	}
	return dnsName
}

// resolveTSNode finds the tailscale node matching want (a device's
// TailscaleHost()) within nodes, case-insensitively on HostName first,
// then on the first label of DNSName. ok=false means the device simply
// isn't present in tailscale's view of the fleet at all (or nodes is nil
// because the tailscale call itself failed) -- callers must treat that as
// ReachUnknown, never ReachOffline; "we don't know" and "known down" are
// different states.
//
// This deliberately does NOT assume want, or any node's HostName, is a
// safe ssh destination -- tailscale peer names can contain spaces (e.g. a
// phone named "Pixel 9 Pro"), and matching must not choke or misfire on
// that shape. It's simple string comparison the whole way, never used to
// build a command.
func resolveTSNode(want string, nodes map[string]TSNode) (TSNode, bool) {
	if want == "" {
		return TSNode{}, false
	}
	for _, n := range nodes {
		if strings.EqualFold(n.HostName, want) {
			return n, true
		}
	}
	for _, n := range nodes {
		if strings.EqualFold(tsFirstDNSLabel(n.DNSName), want) {
			return n, true
		}
	}
	return TSNode{}, false
}

// defaultProbeTimeout is the ceiling used when a caller invokes TailscaleStatus
// or ProbeOne against a context with no deadline (e.g. a unit test calling
// them directly against context.Background()). In production, Cache always
// wraps ctx with config.FleetConfig.ProbeTimeout() before calling either, so
// this default is a defensive fallback rather than the normal path.
const defaultProbeTimeout = 6 * time.Second

// probeCallTimeout derives the timeout duration to hand to
// remotex.Runner.Run / remotex.RunRemote (a value distinct from, but meant
// to agree with, ctx's own deadline) from ctx's deadline when the caller
// has set one, falling back to defaultProbeTimeout otherwise.
func probeCallTimeout(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 {
			return remaining
		}
		// Deadline already passed; hand back a minimal positive duration
		// and let ctx.Done() (which Runner implementations must honor)
		// fail the call immediately rather than passing 0/negative
		// through to something that might interpret it as "no timeout".
		return time.Millisecond
	}
	return defaultProbeTimeout
}
