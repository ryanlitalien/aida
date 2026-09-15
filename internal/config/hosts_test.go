package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHostsConfig writes ~/.aida/config.yaml under the test's HOME (see
// t.Setenv in each test) with the given raw YAML body. Mirrors
// writeCalendarConfig in calendar_test.go.
func writeHostsConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ConfigDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func TestLoadHosts_MissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no config.yaml at all

	hosts, err := LoadHosts()
	if err != nil {
		t.Fatalf("LoadHosts() error = %v, want nil (missing file is not an error)", err)
	}
	if !hosts.IsEmpty() {
		t.Fatalf("hosts = %+v, want empty", hosts)
	}
}

func TestLoadHosts_MissingBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHostsConfig(t, home, "active_profile: auto\n")

	hosts, err := LoadHosts()
	if err != nil {
		t.Fatalf("LoadHosts() error = %v, want nil", err)
	}
	if !hosts.IsEmpty() {
		t.Fatalf("hosts = %+v, want empty", hosts)
	}
}

func TestLoadHosts_ParsesEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHostsConfig(t, home, `hosts:
  - name: build-box
    description: runs long background agent jobs
    reach: "ssh alias: build-box"
  - name: nas
    description: file storage, no shell access
`)

	hosts, err := LoadHosts()
	if err != nil {
		t.Fatalf("LoadHosts() error = %v", err)
	}
	if hosts.IsEmpty() {
		t.Fatal("hosts should not be empty")
	}
	if len(hosts.Hosts) != 2 {
		t.Fatalf("Hosts = %+v, want 2 entries", hosts.Hosts)
	}
	if hosts.Hosts[0].Name != "build-box" || hosts.Hosts[0].Reach != "ssh alias: build-box" {
		t.Errorf("Hosts[0] = %+v", hosts.Hosts[0])
	}
	if hosts.Hosts[1].Name != "nas" || hosts.Hosts[1].Reach != "" {
		t.Errorf("Hosts[1] = %+v, want empty Reach", hosts.Hosts[1])
	}
}

// TestHostsConfig_ForPrompt_Empty covers the "no hosts configured" case
// that the Jarvis system prompt golden test depends on: an empty
// HostsConfig (including a nil pointer) must render no block at all,
// not an empty "Known machines:" header.
func TestHostsConfig_ForPrompt_Empty(t *testing.T) {
	var nilHosts *HostsConfig
	if got := nilHosts.ForPrompt(); got != "" {
		t.Errorf("nil HostsConfig.ForPrompt() = %q, want \"\"", got)
	}
	if got := (&HostsConfig{}).ForPrompt(); got != "" {
		t.Errorf("empty HostsConfig.ForPrompt() = %q, want \"\"", got)
	}
}

func TestHostsConfig_ForPrompt_RendersEntries(t *testing.T) {
	hosts := &HostsConfig{Hosts: []HostEntry{
		{Name: "build-box", Description: "runs long background agent jobs", Reach: "ssh alias: build-box"},
		{Name: "nas", Description: "file storage, no shell access"},
	}}
	prose := hosts.ForPrompt()
	for _, want := range []string{"Known machines:", "build-box", "runs long background agent jobs", "ssh alias: build-box", "nas", "file storage, no shell access"} {
		if !strings.Contains(prose, want) {
			t.Errorf("ForPrompt() missing %q in:\n%s", want, prose)
		}
	}
}

// TestHostsConfig_ForPrompt_SkipsUnnamedEntry guards against a malformed
// entry (empty name) silently producing a nameless, confusing line.
func TestHostsConfig_ForPrompt_SkipsUnnamedEntry(t *testing.T) {
	hosts := &HostsConfig{Hosts: []HostEntry{
		{Name: "", Description: "should not appear"},
		{Name: "build-box", Description: "should appear"},
	}}
	prose := hosts.ForPrompt()
	if strings.Contains(prose, "should not appear") {
		t.Errorf("ForPrompt() rendered an unnamed entry: %q", prose)
	}
	if !strings.Contains(prose, "should appear") {
		t.Errorf("ForPrompt() missing the named entry: %q", prose)
	}
}
