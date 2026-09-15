package roster

import "fmt"

// factories maps an Entry.Kind to its backend constructor. Adding a new
// backend kind is one implementation of the Backend interface plus one
// line here.
var factories = map[string]func(*Entry, Deps) (Backend, error){
	KindSubagent: newSubagentBackend,
	KindSource:   newSourceBackend,
	KindMCP:      newMCPBackend,
	KindJob:      newJobBackend,
}

// BackendFor constructs the Backend that answers for e, using deps for any
// shared runtime handles the backend needs.
func (r *Roster) BackendFor(e *Entry, deps Deps) (Backend, error) {
	if e.Kind == KindAida {
		return nil, fmt.Errorf("%q is the orchestrator, not a dispatch target", e.Display())
	}
	f, ok := factories[e.Kind]
	if !ok {
		return nil, fmt.Errorf("no backend for kind %q (entry %q)", e.Kind, e.Name)
	}
	return f(e, deps)
}
