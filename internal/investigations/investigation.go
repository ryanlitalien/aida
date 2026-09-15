// Package investigations records cloud managed agent sessions to
// ~/.aida/investigations/ as structured JSON files.
package investigations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

const dirName = "investigations"

// Investigation is the durable record of one cloud investigation session.
type Investigation struct {
	SessionID      string            `json:"session_id"`
	Profile        string            `json:"profile"`
	Question       string            `json:"question"`
	StartedAt      time.Time         `json:"started_at"`
	Status         string            `json:"status"`
	Action         string            `json:"action"`
	Strategy       string            `json:"strategy"`
	Entities       []string          `json:"entities,omitempty"`
	ResolvedValues map[string]string `json:"resolved_values,omitempty"`
	AgentID        string            `json:"agent_id"`
	EnvironmentID  string            `json:"environment_id"`
	Report         string            `json:"report,omitempty"`
}

// Dir returns ~/.aida/investigations/.
func Dir() string {
	return filepath.Join(config.Dir(), dirName)
}

// Save writes the investigation to ~/.aida/investigations/<session-id>.json.
func Save(inv *Investigation) (string, error) {
	if err := os.MkdirAll(Dir(), 0755); err != nil {
		return "", err
	}
	path := filepath.Join(Dir(), inv.SessionID+".json")
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

// Load reads an investigation by session ID.
func Load(sessionID string) (*Investigation, error) {
	path := filepath.Join(Dir(), sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inv Investigation
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

// List returns session IDs of recorded investigations sorted newest first.
func List() ([]string, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".json"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// Latest returns the most recently recorded investigation, or nil if none.
func Latest() (*Investigation, error) {
	ids, err := List()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return Load(ids[0])
}

// SaveReport writes the report as a markdown artifact and returns the path.
func SaveReport(inv *Investigation) (string, error) {
	if inv.Report == "" {
		return "", fmt.Errorf("no report content")
	}
	if err := os.MkdirAll(Dir(), 0755); err != nil {
		return "", err
	}
	path := filepath.Join(Dir(), inv.SessionID+"-report.md")
	if err := os.WriteFile(path, []byte(inv.Report), 0644); err != nil {
		return "", err
	}
	return path, nil
}

// UpdateStatus updates the status field of an existing investigation.
func UpdateStatus(sessionID, status string) error {
	inv, err := Load(sessionID)
	if err != nil {
		return fmt.Errorf("loading investigation: %w", err)
	}
	inv.Status = status
	_, err = Save(inv)
	return err
}
