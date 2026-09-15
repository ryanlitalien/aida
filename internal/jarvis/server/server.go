// Package server is the HTTP surface for Jarvis: status panel, /jarvis/ask
// endpoint, and /jarvis/health. Mounted by `aida serve` on port 1610 (Earth-1610).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/jarvis"
)

// New mounts Jarvis HTTP routes onto mux. The Assistant must already be
// constructed.
func New(mux *http.ServeMux, a *jarvis.Assistant) *Server {
	s := &Server{a: a, started: time.Now()}
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/jarvis/ask", s.handleAsk)
	mux.HandleFunc("/jarvis/health", s.handleHealth)
	mux.HandleFunc("/jarvis/activity", s.handleActivity)
	return s
}

type Server struct {
	a       *jarvis.Assistant
	started time.Time

	mu        sync.Mutex
	lastQuery string
	lastReply string
}

type askRequest struct {
	Query string `json:"query"`
	Speak *bool  `json:"speak,omitempty"` // default true
}

type askResponse struct {
	Reply string `json:"reply"`
	Spoke bool   `json:"spoke"`
	Took  string `json:"took"`
}

// TODO(decommission): /jarvis/ask is disabled pending removal. It ran a
// full LLM turn with tool dispatch on an unauthenticated loopback route,
// which any site the user visits can reach via CSRF - loopback is not a
// boundary against a browser. Its only caller was the ask form on the
// status panel, removed alongside this. Delete handleAsk, askRequest,
// askResponse, and the route registration once nothing has 410'd for a
// release or two. Use `aida jarvis ask` (in-process) instead.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "disabled; use `aida jarvis ask`", http.StatusGone)
}

// handleActivity returns the live "what is Jarvis doing right now" snapshot -
// the currently-executing tool plus the recent-turns ring - polled by the
// status page's activity card.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(s.a.ActivitySnapshot())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"uptime":  time.Since(s.started).Round(time.Second).String(),
		"version": "jarvis-v1",
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	q, reply := s.lastQuery, s.lastReply
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, indexHTML, time.Since(s.started).Round(time.Second), q, reply)
}

const indexHTML = `<!doctype html>
<html><head><title>JARVIS - Earth-1610</title>
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="icon" href="/favicon.ico" sizes="32x32">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<style>
body { font-family: -apple-system, system-ui, sans-serif; background:#0b1320;
  color:#cfe2ff; max-width:680px; margin:48px auto; padding:0 24px; }
h1 { color:#7cc5ff; letter-spacing:.05em; }
.card { background:#152035; padding:16px 20px; border-radius:10px; margin:14px 0; }
code { color:#ffd77a; }
.muted { color:#6c8bb8; font-size:.85em; }
nav a { color:#7cc5ff; text-decoration:none; font-weight:550; }
nav a.active { color:#cfe2ff; cursor:default; }
</style></head><body>
<h1>J.A.R.V.I.S.</h1>
<nav>
  <a class="active">Jarvis</a> &middot;
  <a href="/tasks">Tasks</a> &middot;
  <a href="/runs">Runs</a> &middot;
  <a href="/dashboard">Dashboard</a> &middot;
  <a href="/bifrost">Bifrost</a> &middot;
  <a href="/menu">Menu</a> &middot;
  <a href="/habits">Habits</a>
</nav>
<div class="card">
  <div class="muted">uptime</div>
  <div>%s</div>
</div>
<div class="card">
  <div class="muted">current activity</div>
  <div id="activity">idle</div>
  <div class="muted" style="margin-top:12px">recent turns</div>
  <div id="recent"></div>
</div>
<div class="card">
  <div class="muted">ask jarvis</div>
  <div>use <code>aida jarvis ask "..."</code> from a terminal</div>
</div>
<div class="card">
  <div class="muted">last query</div><div>%s</div>
  <div class="muted" style="margin-top:10px">last reply</div><div>%s</div>
</div>
<p class="muted">routes: <code>GET /jarvis/health</code> · <code>GET /tasks</code></p>
<script>
function escapeHtml(s){return (s||'').replace(/[&<>]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;'}[c];});}
async function pollActivity(){
  try{
    const r = await fetch('/jarvis/activity');
    const j = await r.json();
    const cur = document.getElementById('activity');
    if(j.current_tool){
      const secs = j.current_for_ms ? ' ('+Math.round(j.current_for_ms/1000)+'s)' : '';
      cur.innerHTML = '<span style="color:#ffd77a">&#9654; '+escapeHtml(j.current_tool)+secs+'</span>';
    } else {
      cur.textContent = 'idle';
    }
    const rec = document.getElementById('recent');
    rec.innerHTML = '';
    (j.recent||[]).forEach(function(t){
      const d = document.createElement('div');
      d.style.marginBottom = '8px';
      const tools = (t.tools && t.tools.length) ? ' <span class="muted">['+escapeHtml(t.tools.join(', '))+']</span>' : '';
      const body = t.error ? ('<span style="color:#ff9a9a">'+escapeHtml(t.error)+'</span>') : escapeHtml(t.reply||'');
      d.innerHTML = '<b>'+escapeHtml(t.query||'')+'</b>'+tools+'<br><span class="muted">'+body+'</span>';
      rec.appendChild(d);
    });
  }catch(e){}
}
setInterval(pollActivity, 1500);
pollActivity();
</script>
</body></html>`

// Stop is a hook for future graceful shutdown work.
func (s *Server) Stop(_ context.Context) error { _ = os.Stderr; return nil }
