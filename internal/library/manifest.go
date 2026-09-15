package library

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ManifestFile is the well-known filename inside a library root.
const ManifestFile = "library.yaml"

// LoadManifest reads <rootPath>/library.yaml and returns the parsed manifest.
// If the file does not exist, an empty manifest is returned (an empty root is
// still valid -- it just contributes nothing).
func LoadManifest(rootPath string) (*Manifest, error) {
	path := filepath.Join(rootPath, ManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Manifest{Version: 1}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if m.Version == 0 {
		m.Version = 1
	}
	return &m, nil
}

// SaveManifest writes a manifest to <rootPath>/library.yaml.
func SaveManifest(rootPath string, m *Manifest) error {
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return fmt.Errorf("creating root dir: %w", err)
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(rootPath, ManifestFile), data, 0644)
}
