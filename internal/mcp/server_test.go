package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
)

// newTestServer builds an MCP Server backed by a temp-dir brain.
// Auto-sync is off (zero BrainConfig) so CallTool does not try to git push.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	b, err := brain.Open(dir, "test", "", "")
	if err != nil {
		t.Fatalf("brain.Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return &Server{brain: b, cfg: &config.Config{}, profile: "test"}
}

func callTool(s *Server, name string, args map[string]interface{}) toolResult {
	return s.callTool(toolCallParams{Name: name, Arguments: args})
}

func TestCallTool_TasksAdd_Then_Edit(t *testing.T) {
	s := newTestServer(t)

	// Create a task.
	addRes := callTool(s, "tasks_add", map[string]interface{}{
		"title": "smoke test task",
		"tags":  "p3,project:mcp-edit",
	})
	if addRes.IsError {
		t.Fatalf("tasks_add errored: %v", addRes)
	}

	// Must be listed in tasks_list.
	listRes := callTool(s, "tasks_list", map[string]interface{}{"tag": "mcp-edit"})
	if !strings.Contains(listRes.Content[0].Text, "smoke test task") {
		t.Fatalf("expected task in list, got: %s", listRes.Content[0].Text)
	}

	// Edit: rename + add tag.
	editRes := callTool(s, "tasks_edit", map[string]interface{}{
		"slug":     "smoke-test-task",
		"title":    "smoke test renamed",
		"add_tags": "p1",
	})
	if editRes.IsError {
		t.Fatalf("tasks_edit errored: %s", editRes.Content[0].Text)
	}
	if !strings.Contains(editRes.Content[0].Text, "smoke test renamed") {
		t.Errorf("edit response should show new title: %s", editRes.Content[0].Text)
	}

	// tasks_get should return JSON with updated fields.
	getRes := callTool(s, "tasks_get", map[string]interface{}{"slug": "smoke-test-task"})
	if getRes.IsError {
		t.Fatalf("tasks_get errored: %s", getRes.Content[0].Text)
	}
	var rec brain.TaskRecord
	if err := json.Unmarshal([]byte(getRes.Content[0].Text), &rec); err != nil {
		t.Fatalf("unmarshal tasks_get output: %v (raw: %s)", err, getRes.Content[0].Text)
	}
	if rec.Title != "smoke test renamed" {
		t.Errorf("title = %q, want %q", rec.Title, "smoke test renamed")
	}
	hasP1 := false
	for _, tg := range rec.Tags {
		if tg == "p1" {
			hasP1 = true
		}
	}
	if !hasP1 {
		t.Errorf("expected p1 in tags %v", rec.Tags)
	}
}

func TestCallTool_TasksDone_HashRef(t *testing.T) {
	s := newTestServer(t)

	addRes := callTool(s, "tasks_add", map[string]interface{}{"title": "done via hashref"})
	if addRes.IsError {
		t.Fatalf("tasks_add: %v", addRes)
	}

	// Fetch the assigned task_id via tasks_list and extract the first #N.
	listRes := callTool(s, "tasks_list", map[string]interface{}{})
	text := listRes.Content[0].Text
	if !strings.HasPrefix(text, "#") {
		t.Fatalf("expected list to start with #N, got: %s", text)
	}
	// Grab "#N" prefix.
	end := strings.IndexByte(text, ' ')
	if end < 0 {
		t.Fatalf("could not parse task ref from %q", text)
	}
	ref := text[:end]

	// tasks_done must accept the #N form.
	doneRes := callTool(s, "tasks_done", map[string]interface{}{"slug": ref})
	if doneRes.IsError {
		t.Fatalf("tasks_done errored: %s", doneRes.Content[0].Text)
	}
	if !strings.Contains(doneRes.Content[0].Text, "Done:") {
		t.Errorf("unexpected done text: %s", doneRes.Content[0].Text)
	}
}

func TestCallTool_TasksReopen(t *testing.T) {
	s := newTestServer(t)

	callTool(s, "tasks_add", map[string]interface{}{"title": "reopen me"})
	callTool(s, "tasks_done", map[string]interface{}{"slug": "reopen-me"})

	reopenRes := callTool(s, "tasks_reopen", map[string]interface{}{"slug": "reopen-me"})
	if reopenRes.IsError {
		t.Fatalf("tasks_reopen errored: %s", reopenRes.Content[0].Text)
	}
	if !strings.Contains(reopenRes.Content[0].Text, "Reopened:") {
		t.Errorf("unexpected reopen text: %s", reopenRes.Content[0].Text)
	}

	// After reopen, task should appear in default list (open-only).
	listRes := callTool(s, "tasks_list", map[string]interface{}{})
	if !strings.Contains(listRes.Content[0].Text, "reopen me") {
		t.Errorf("reopened task should be in open list: %s", listRes.Content[0].Text)
	}
}

func TestCallTool_TasksShow_StripsFrontmatter(t *testing.T) {
	s := newTestServer(t)

	callTool(s, "tasks_add", map[string]interface{}{"title": "show me"})
	showRes := callTool(s, "tasks_show", map[string]interface{}{"slug": "show-me"})
	if showRes.IsError {
		t.Fatalf("tasks_show errored: %s", showRes.Content[0].Text)
	}
	if strings.Contains(showRes.Content[0].Text, "---") {
		t.Errorf("frontmatter should be stripped, got: %s", showRes.Content[0].Text)
	}
	if !strings.Contains(showRes.Content[0].Text, "show me") {
		t.Errorf("expected title in body, got: %s", showRes.Content[0].Text)
	}
}

func TestCallTool_TasksEdit_RequiresSlug(t *testing.T) {
	s := newTestServer(t)
	res := callTool(s, "tasks_edit", map[string]interface{}{})
	if !res.IsError {
		t.Error("expected error when slug is missing")
	}
}

func TestCallTool_TasksEdit_StatusTransition(t *testing.T) {
	s := newTestServer(t)
	callTool(s, "tasks_add", map[string]interface{}{"title": "status flip"})

	res := callTool(s, "tasks_edit", map[string]interface{}{
		"slug":   "status-flip",
		"status": "done",
	})
	if res.IsError {
		t.Fatalf("tasks_edit status=done errored: %s", res.Content[0].Text)
	}

	getRes := callTool(s, "tasks_get", map[string]interface{}{"slug": "status-flip"})
	var rec brain.TaskRecord
	json.Unmarshal([]byte(getRes.Content[0].Text), &rec)
	if !rec.Completed {
		t.Errorf("expected completed=true after status=done edit, got %+v", rec)
	}
}

func TestStripFrontmatter(t *testing.T) {
	in := "---\nstatus: open\ntags:\n- p1\n---\n\n# Title\n\nBody text"
	want := "# Title\n\nBody text"
	if got := stripFrontmatter(in); got != want {
		t.Errorf("stripFrontmatter() = %q, want %q", got, want)
	}

	// No frontmatter - return as-is.
	if got := stripFrontmatter("just body"); got != "just body" {
		t.Errorf("stripFrontmatter(plain) = %q", got)
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV("a, b , ,c")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitCSV len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("splitCSV[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProxy_ForwardsToolCall(t *testing.T) {
	want := "PROXIED-STATS"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/call" || r.Method != http.MethodPost {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		var p toolCallParams
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode forwarded params: %v", err)
		}
		if p.Name != "brain_stats" {
			t.Errorf("forwarded name = %q, want brain_stats", p.Name)
		}
		json.NewEncoder(w).Encode(textResult(want))
	}))
	defer daemon.Close()

	s := &Server{proxy: true, daemonURL: daemon.URL, httpc: daemon.Client()}
	res := s.callTool(toolCallParams{Name: "brain_stats"})
	if res.IsError {
		t.Fatalf("unexpected error result: %v", res)
	}
	if len(res.Content) == 0 || res.Content[0].Text != want {
		t.Errorf("proxied result = %+v, want text %q", res, want)
	}
}

func TestProxy_RelaysDaemonToolError(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(errorResult("boom"))
	}))
	defer daemon.Close()

	s := &Server{proxy: true, daemonURL: daemon.URL, httpc: daemon.Client()}
	res := s.callTool(toolCallParams{Name: "tasks_get", Arguments: map[string]interface{}{"slug": "x"}})
	if !res.IsError {
		t.Error("expected the daemon's IsError result to be relayed")
	}
}

func TestProxy_FallsBackToLocalWhenDaemonDown(t *testing.T) {
	s := newTestServer(t) // real temp-dir brain
	s.proxy = true
	s.daemonURL = "http://127.0.0.1:1" // nothing listening → connection refused
	s.httpc = &http.Client{Timeout: 2 * time.Second}

	res := s.callTool(toolCallParams{Name: "brain_stats"})
	if res.IsError {
		t.Fatalf("expected local fallback to succeed, got: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "Lessons:") {
		t.Errorf("expected in-process brain_stats output, got: %s", res.Content[0].Text)
	}
}

func TestHTTPHandler_DispatchesLocally(t *testing.T) {
	s := newTestServer(t)
	callTool(s, "tasks_add", map[string]interface{}{"title": "via http handler"})

	handler := s.HTTPHandler()
	body, _ := json.Marshal(toolCallParams{Name: "tasks_list", Arguments: map[string]interface{}{}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out toolResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.IsError || len(out.Content) == 0 || !strings.Contains(out.Content[0].Text, "via http handler") {
		t.Errorf("expected task in list, got: %+v", out)
	}
}
