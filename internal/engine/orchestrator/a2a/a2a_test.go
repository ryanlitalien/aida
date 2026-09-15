package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	a2aclient "trpc.group/trpc-go/trpc-a2a-go/client"
	a2aprotocol "trpc.group/trpc-go/trpc-a2a-go/protocol"
	a2aserver "trpc.group/trpc-go/trpc-a2a-go/server"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
)

// stubRunnable is a minimal orchestrator.Runnable that records the
// input it receives and returns a canned string.
type stubRunnable struct {
	reply     string
	lastInput atomic.Pointer[string]
}

func (s *stubRunnable) Run(_ context.Context, input string, _ ...orchestrator.RunOption) (*orchestrator.RunResult, error) {
	in := input
	s.lastInput.Store(&in)
	return &orchestrator.RunResult{FinalOutput: s.reply}, nil
}

func TestExtractText(t *testing.T) {
	m := a2aprotocol.NewMessage(
		a2aprotocol.MessageRoleUser,
		[]a2aprotocol.Part{
			a2aprotocol.NewTextPart("hello "),
			a2aprotocol.NewTextPart("world"),
		},
	)
	if got := extractText(m); got != "hello \nworld" {
		t.Errorf("extractText = %q", got)
	}
}

func TestExtractTextEmpty(t *testing.T) {
	m := a2aprotocol.NewMessage(a2aprotocol.MessageRoleUser, nil)
	if got := extractText(m); got != "" {
		t.Errorf("extractText = %q, want empty", got)
	}
}

// TestServeNilRunnable verifies the constructor rejects nil.
func TestServeNilRunnable(t *testing.T) {
	if _, err := Serve(nil, a2aserver.AgentCard{Name: "x"}); err == nil {
		t.Error("expected error for nil runnable")
	}
}

// TestRoundtripServeAndClient wires Serve() + NewRemoteAgent() through
// an httptest server, proving that:
//   - an orchestrator.Runnable can be served as an A2A endpoint,
//   - a RemoteAgent can consume that endpoint as another Runnable,
//   - the input/output round-trip preserves text.
func TestRoundtripServeAndClient(t *testing.T) {
	stub := &stubRunnable{reply: "hello from stub"}
	card := a2aserver.AgentCard{
		Name:               "stub",
		Description:        "test stub",
		Version:            "0.1",
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
	}
	srv, err := Serve(stub, card)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	remote, err := NewRemoteAgent("remote-stub", ts.URL)
	if err != nil {
		t.Fatalf("NewRemoteAgent: %v", err)
	}

	res, err := remote.Run(context.Background(), "ping")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "hello from stub" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
	if res.HandoffPath[0] != "remote-stub" {
		t.Errorf("HandoffPath = %v", res.HandoffPath)
	}
	if in := stub.lastInput.Load(); in == nil || *in != "ping" {
		t.Errorf("stub saw input %v, want ping", in)
	}
}

// TestRemoteAgentWrapsAsRunnable ensures RemoteAgent satisfies the
// orchestrator.Runnable interface at compile time and can be nested
// inside a SequentialAgent.
func TestRemoteAgentWrapsAsRunnable(t *testing.T) {
	stub := &stubRunnable{reply: "from-remote"}
	srv, err := Serve(stub, a2aserver.AgentCard{
		Name:               "stub",
		Description:        "test",
		Version:            "0.1",
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	remote, err := NewRemoteAgent("remote", ts.URL)
	if err != nil {
		t.Fatalf("NewRemoteAgent: %v", err)
	}

	// Compile-time check.
	var _ orchestrator.Runnable = remote

	// Build a Sequential graph with the remote agent as its sole child.
	seq := &orchestrator.SequentialAgent{
		Name:     "with-remote",
		Children: []orchestrator.Runnable{remote},
	}
	res, err := seq.Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "from-remote" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
}

// TestAgentCardEndpoint verifies that the A2A server also exposes the
// agent card at the standard well-known path, proving the card is
// serialized and wired to the right endpoint.
func TestAgentCardEndpoint(t *testing.T) {
	stub := &stubRunnable{reply: ""}
	card := a2aserver.AgentCard{
		Name:               "card-test",
		Description:        "serves a card",
		Version:            "0.1.0",
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
	}
	srv, err := Serve(stub, card)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET card: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("card status = %d", resp.StatusCode)
	}
	var got a2aserver.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "card-test" {
		t.Errorf("card.Name = %q", got.Name)
	}
	if got.Description != "serves a card" {
		t.Errorf("card.Description = %q", got.Description)
	}
}

// TestRemoteAgentNilClient returns a clear error rather than panicking.
func TestRemoteAgentNilClient(t *testing.T) {
	r := &RemoteAgent{Name: "broken"}
	if _, err := r.Run(context.Background(), "x"); err == nil {
		t.Error("expected error for nil client")
	} else if !strings.Contains(err.Error(), "nil") {
		t.Errorf("err = %v", err)
	}
}

// Compile-time check that we didn't miss an import.
var _ = a2aclient.NewA2AClient
