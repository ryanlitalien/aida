package a2a

import (
	"context"
	"errors"
	"fmt"
	"strings"

	a2aclient "trpc.group/trpc-go/trpc-a2a-go/client"
	a2aprotocol "trpc.group/trpc-go/trpc-a2a-go/protocol"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
)

// RemoteAgent is a thin orchestrator.Runnable that forwards every Run
// to a remote A2A endpoint. This lets a local orchestrator graph
// include remote agents interchangeably with in-process agents.
type RemoteAgent struct {
	// Name is used in traces and composite HandoffPath entries.
	Name string
	// Client is the underlying A2A client. Construct with
	// NewRemoteAgent or supply your own for custom opts.
	Client *a2aclient.A2AClient
}

// NewRemoteAgent constructs a RemoteAgent pointed at agentURL.
func NewRemoteAgent(name, agentURL string, opts ...a2aclient.Option) (*RemoteAgent, error) {
	if name == "" {
		return nil, errors.New("a2a: RemoteAgent name is empty")
	}
	c, err := a2aclient.NewA2AClient(agentURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("a2a: client: %w", err)
	}
	return &RemoteAgent{Name: name, Client: c}, nil
}

// Run implements orchestrator.Runnable. The input string becomes a
// single TextPart in a user-role Message; the remote agent's reply
// text becomes FinalOutput. Non-text parts in the reply are ignored.
func (r *RemoteAgent) Run(
	ctx context.Context,
	input string,
	_ ...orchestrator.RunOption,
) (*orchestrator.RunResult, error) {
	if r.Client == nil {
		return nil, errors.New("a2a: RemoteAgent.Client is nil")
	}
	msg := a2aprotocol.NewMessage(
		a2aprotocol.MessageRoleUser,
		[]a2aprotocol.Part{a2aprotocol.NewTextPart(input)},
	)
	res, err := r.Client.SendMessage(ctx, a2aprotocol.SendMessageParams{Message: msg})
	if err != nil {
		return nil, fmt.Errorf("a2a: send: %w", err)
	}
	if res == nil || res.Result == nil {
		return &orchestrator.RunResult{HandoffPath: []string{r.Name}}, nil
	}
	text := ""
	if m, ok := res.Result.(*a2aprotocol.Message); ok {
		text = extractText(*m)
	}
	return &orchestrator.RunResult{
		FinalOutput: strings.TrimSpace(text),
		HandoffPath: []string{r.Name},
	}, nil
}
