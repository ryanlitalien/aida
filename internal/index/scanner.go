package index

import (
	"os"
	"path/filepath"
	"time"
)

// ScannedFolder represents a directory found during scanning that contains
// a documentation file (CLAUDE.md, AGENT.md, or README.md).
type ScannedFolder struct {
	Path    string    // absolute path to the directory
	DocFile string    // "CLAUDE.md", "AGENT.md", "README.md", or ""
	ModTime time.Time // modification time of the doc file
}

// docFileNames lists the documentation files we look for, in priority order.
var docFileNames = []string{"CLAUDE.md", "AGENT.md", "README.md"}

// ScanPaths walks each of the given paths and returns directories that
// contain a CLAUDE.md or README.md file. Only the first level of each
// child directory is checked (non-recursive within each child).
func ScanPaths(paths []string) ([]ScannedFolder, error) {
	var results []ScannedFolder
	seen := make(map[string]bool)

	for _, root := range paths {
		entries, err := os.ReadDir(root)
		if err != nil {
			// Skip paths that cannot be read (e.g. don't exist yet).
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			dirPath := filepath.Join(root, entry.Name())
			if seen[dirPath] {
				continue
			}

			folder := scanDir(dirPath)
			if folder != nil {
				seen[dirPath] = true
				results = append(results, *folder)
			}
		}
	}

	return results, nil
}

// scanDir checks a single directory for the presence of a doc file.
func scanDir(dir string) *ScannedFolder {
	for _, name := range docFileNames {
		fullPath := filepath.Join(dir, name)
		info, err := os.Stat(fullPath)
		if err == nil && !info.IsDir() {
			return &ScannedFolder{
				Path:    dir,
				DocFile: name,
				ModTime: info.ModTime(),
			}
		}
	}
	return nil
}
