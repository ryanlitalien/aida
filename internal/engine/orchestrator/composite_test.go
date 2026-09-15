package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// makeAgent returns an Agent backed by a scriptLLM that returns a
// single end_turn with the given text on every Complete call - useful
// for building composite test fixtures.
func makeAgent(name, reply string) *Agent {
	return &Agent{
		Name: name,
		Model: &scriptLLM{
			responses: []model.Response{
				{
					StopReason: model.StopEndTurn,
					Blocks:     []model.Block{model.TextBlock(reply)},
				},
			},
		},
	}
}

// capturingAgent returns an Agent whose LLM records the input it
// received so tests can assert what a composite passed through.
type capturingAgent struct {
	*Agent
	llm *scriptLLM
}

func makeCapturing(name, reply string) *capturingAgent {
	llm := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopEndTurn,
				Blocks:     []model.Block{model.TextBlock(reply)},
			},
		},
	}
	return &capturingAgent{
		Agent: &Agent{Name: name, Model: llm},
		llm:   llm,
	}
}

func (c *capturingAgent) firstInput() string {
	if len(c.llm.calls) == 0 {
		return ""
	}
	for _, m := range c.llm.calls[0].Messages {
		if m.Role != model.RoleUser {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == model.BlockText {
				return b.Text
			}
		}
	}
	return ""
}

// TestSequentialAgentPipesOutputForward ensures child N's output
// becomes child N+1's input.
func TestSequentialAgentPipesOutputForward(t *testing.T) {
	a := makeCapturing("a", "output-from-a")
	b := makeCapturing("b", "output-from-b")
	c := makeCapturing("c", "output-from-c")

	seq := &SequentialAgent{
		Name:     "pipeline",
		Children: []Runnable{a.Agent, b.Agent, c.Agent},
	}
	res, err := seq.Run(context.Background(), "start")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "output-from-c" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
	if a.firstInput() != "start" {
		t.Errorf("a saw input %q, want 'start'", a.firstInput())
	}
	if b.firstInput() != "output-from-a" {
		t.Errorf("b saw input %q, want 'output-from-a'", b.firstInput())
	}
	if c.firstInput() != "output-from-b" {
		t.Errorf("c saw input %q, want 'output-from-b'", c.firstInput())
	}
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3", res.Turns)
	}
}

// TestSequentialAgentErrorAborts ensures an early child error aborts
// the pipeline and later children are not run.
func TestSequentialAgentErrorAborts(t *testing.T) {
	var counter int64
	erroring := &Agent{
		Name:  "err",
		Model: &scriptLLM{}, // empty script: exhausted → error on first call
	}
	b := &Agent{
		Name: "b",
		Model: &scriptLLM{
			responses: []model.Response{
				{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("ok")}},
			},
		},
		Tools: []Tool{countingTool("noop", &counter)},
	}
	seq := &SequentialAgent{Name: "p", Children: []Runnable{erroring, b}}
	if _, err := seq.Run(context.Background(), "start"); err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt64(&counter) != 0 {
		t.Errorf("b should not have been run, counter = %d", counter)
	}
}

// TestSequentialAgentEmpty is an error, not a silent no-op.
func TestSequentialAgentEmpty(t *testing.T) {
	seq := &SequentialAgent{Name: "p"}
	if _, err := seq.Run(context.Background(), "x"); err == nil {
		t.Error("expected error for empty children")
	}
}

// TestParallelAgentFanOut: all children see the same input, all run
// concurrently, and the default merge concatenates outputs.
func TestParallelAgentFanOut(t *testing.T) {
	a := makeCapturing("a", "A")
	b := makeCapturing("b", "B")
	c := makeCapturing("c", "C")

	par := &ParallelAgent{
		Name:     "fanout",
		Children: []Runnable{a.Agent, b.Agent, c.Agent},
	}
	res, err := par.Run(context.Background(), "shared-input")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.firstInput() != "shared-input" || b.firstInput() != "shared-input" || c.firstInput() != "shared-input" {
		t.Errorf("children did not all see shared input: %q %q %q", a.firstInput(), b.firstInput(), c.firstInput())
	}
	if !strings.Contains(res.FinalOutput, "A") || !strings.Contains(res.FinalOutput, "B") || !strings.Contains(res.FinalOutput, "C") {
		t.Errorf("FinalOutput = %q; want all three", res.FinalOutput)
	}
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3 (one per child)", res.Turns)
	}
}

// TestParallelAgentActuallyParallel is a timing test: three children
// each sleep 50ms; the composite should finish in ~50ms, not 150ms.
func TestParallelAgentActuallyParallel(t *testing.T) {
	slow := func(name string) *Agent {
		return &Agent{
			Name:  name,
			Model: &sleepyLLM{delay: 50 * time.Millisecond, reply: name},
		}
	}
	par := &ParallelAgent{
		Name:     "timing",
		Children: []Runnable{slow("x"), slow("y"), slow("z")},
	}
	start := time.Now()
	res, err := par.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 130*time.Millisecond {
		t.Errorf("elapsed %v - children appear to be running serially", elapsed)
	}
	if !strings.Contains(res.FinalOutput, "x") {
		t.Errorf("missing x output: %q", res.FinalOutput)
	}
}

// TestParallelAgentCustomMerge uses a caller-supplied merge.
func TestParallelAgentCustomMerge(t *testing.T) {
	par := &ParallelAgent{
		Name:     "custom",
		Children: []Runnable{makeAgent("a", "A"), makeAgent("b", "B")},
		Merge: func(rs []*RunResult) string {
			parts := make([]string, len(rs))
			for i, r := range rs {
				parts[i] = "[" + r.FinalOutput + "]"
			}
			return strings.Join(parts, "+")
		},
	}
	res, err := par.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "[A]+[B]" {
		t.Errorf("merged = %q, want [A]+[B]", res.FinalOutput)
	}
}

// TestParallelAgentOneChildFails propagates the error.
func TestParallelAgentOneChildFails(t *testing.T) {
	par := &ParallelAgent{
		Name: "withfailure",
		Children: []Runnable{
			makeAgent("a", "A"),
			&Agent{Name: "boom", Model: &scriptLLM{}}, // script exhausted → error
		},
	}
	if _, err := par.Run(context.Background(), "go"); err == nil {
		t.Error("expected error from failing child")
	}
}

func TestLoopAgentRunsToMaxIters(t *testing.T) {
	var count int64
	agent := &Agent{
		Name: "iterate",
		Model: &callbackLLM{
			fn: func(req model.Request) *model.Response {
				atomic.AddInt64(&count, 1)
				return &model.Response{
					StopReason: model.StopEndTurn,
					Blocks:     []model.Block{model.TextBlock("iteration done")},
				}
			},
		},
	}
	loop := &LoopAgent{Name: "l", Child: agent, MaxIters: 4}
	res, err := loop.Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt64(&count) != 4 {
		t.Errorf("iterations = %d, want 4", count)
	}
	if res.FinalOutput != "iteration done" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
}

func TestLoopAgentShouldStop(t *testing.T) {
	var count int64
	agent := &Agent{
		Name: "iterate",
		Model: &callbackLLM{
			fn: func(req model.Request) *model.Response {
				c := atomic.AddInt64(&count, 1)
				return &model.Response{
					StopReason: model.StopEndTurn,
					Blocks:     []model.Block{model.TextBlock("n=" + itoa(int(c)))},
				}
			},
		},
	}
	loop := &LoopAgent{
		Name:     "l",
		Child:    agent,
		MaxIters: 100,
		ShouldStop: func(res *RunResult) bool {
			return strings.HasSuffix(res.FinalOutput, "=3")
		},
	}
	res, err := loop.Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt64(&count) != 3 {
		t.Errorf("iterations = %d, want 3", count)
	}
	if res.FinalOutput != "n=3" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
}

func TestLoopAgentInvalid(t *testing.T) {
	// No child.
	if _, err := (&LoopAgent{Name: "x", MaxIters: 1}).Run(context.Background(), "x"); err == nil {
		t.Error("expected error for nil child")
	}
	// MaxIters <= 0.
	if _, err := (&LoopAgent{Name: "x", Child: makeAgent("c", "y")}).Run(context.Background(), "x"); err == nil {
		t.Error("expected error for zero MaxIters")
	}
}

// TestComposedAgents builds a small graph: parallel fan-out then
// sequential refinement. Verifies tool-call and turn roll-up.
func TestComposedAgents(t *testing.T) {
	a := makeAgent("a", "alpha")
	b := makeAgent("b", "beta")
	par := &ParallelAgent{Name: "fan", Children: []Runnable{a, b}}
	refine := makeAgent("refine", "refined")
	pipeline := &SequentialAgent{
		Name:     "graph",
		Children: []Runnable{par, refine},
	}
	res, err := pipeline.Run(context.Background(), "start")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "refined" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
	// 2 parallel children + 1 refine = 3 turns.
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3", res.Turns)
	}
}

// ---------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------

type sleepyLLM struct {
	delay time.Duration
	reply string
}

func (s *sleepyLLM) Provider() string { return "sleepy" }
func (s *sleepyLLM) Model() string    { return "sleepy-1" }
func (s *sleepyLLM) Complete(ctx context.Context, _ model.Request) (*model.Response, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &model.Response{
		StopReason: model.StopEndTurn,
		Blocks:     []model.Block{model.TextBlock(s.reply)},
	}, nil
}

type callbackLLM struct {
	fn func(model.Request) *model.Response
}

func (c *callbackLLM) Provider() string { return "cb" }
func (c *callbackLLM) Model() string    { return "cb-1" }
func (c *callbackLLM) Complete(_ context.Context, req model.Request) (*model.Response, error) {
	if c.fn == nil {
		return nil, errors.New("no callback")
	}
	return c.fn(req), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
