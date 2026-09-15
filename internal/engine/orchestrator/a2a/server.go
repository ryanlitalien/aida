// Package a2a wraps trpc-a2a-go to expose an orchestrator.Runnable
// over the A2A protocol and to consume remote A2A endpoints as local
// Runnables.
//
// The goal of this layer is narrow: provide a round-trip between the
// orchestrator's "Runnable takes a string, returns a RunResult"
// contract and A2A's "agent takes a Message of Parts, returns a
// Message of Parts" contract. It does not reimplement A2A itself.
package a2a

import (
	"context"
	"errors"
	"fmt"
	"strings"

	a2aprotocol "trpc.group/trpc-go/trpc-a2a-go/protocol"
	a2aserver "trpc.group/trpc-go/trpc-a2a-go/server"
	a2atm "trpc.group/trpc-go/trpc-a2a-go/taskmanager"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
)

// Serve packages a Runnable as an A2A MessageProcessor backed by an
// in-memory TaskManager, attaches a minimal AgentCard, and returns
// the constructed *A2AServer ready to .Start(addr).
//
// agentURL is the public URL the AgentCard advertises (e.g.
// http://localhost:8080/). It is embedded in the card but not opened
// by Serve - callers control binding via .Start.
func Serve(r orchestrator.Runnable, card a2aserver.AgentCard) (*a2aserver.A2AServer, error) {
	if r == nil {
		return nil, errors.New("a2a: Runnable is nil")
	}
	proc := &runnableProcessor{runnable: r}
	tm, err := a2atm.NewMemoryTaskManager(proc)
	if err != nil {
		return nil, fmt.Errorf("a2a: task manager: %w", err)
	}
	return a2aserver.NewA2AServer(card, tm)
}

// runnableProcessor adapts an orchestrator.Runnable to the
// taskmanager.MessageProcessor interface.
type runnableProcessor struct {
	runnable orchestrator.Runnable
}

// ProcessMessage implements taskmanager.MessageProcessor.
func (p *runnableProcessor) ProcessMessage(
	ctx context.Context,
	message a2aprotocol.Message,
	_ a2atm.ProcessOptions,
	_ a2atm.TaskHandler,
) (*a2atm.MessageProcessingResult, error) {
	input := extractText(message)
	res, err := p.runnable.Run(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("a2a: runnable: %w", err)
	}
	reply := a2aprotocol.NewMessage(
		a2aprotocol.MessageRoleAgent,
		[]a2aprotocol.Part{a2aprotocol.NewTextPart(res.FinalOutput)},
	)
	return &a2atm.MessageProcessingResult{Result: &reply}, nil
}

// extractText joins all text parts in an A2A message. Non-text parts
// (file, data) are ignored - this minimal wrapper only round-trips
// text. Callers that need richer shapes should subclass.
func extractText(m a2aprotocol.Message) string {
	var parts []string
	for _, part := range m.Parts {
		if tp, ok := part.(a2aprotocol.TextPart); ok {
			parts = append(parts, tp.Text)
		} else if tp, ok := part.(*a2aprotocol.TextPart); ok {
			parts = append(parts, tp.Text)
		}
	}
	return strings.Join(parts, "\n")
}
