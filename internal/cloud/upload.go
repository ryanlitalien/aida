package cloud

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
)

// ContextFile holds a file path and its content for context packaging.
type ContextFile struct {
	Path    string // relative path under root dir
	Content string
}

// includedExtensions are file types we include in context packages.
var includedExtensions = map[string]bool{
	".sql":  true,
	".md":   true,
	".yaml": true,
	".yml":  true,
	".txt":  true,
	".json": true,
	".toml": true,
}

// excludedDirs are directories we skip when walking.
var excludedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
}

const (
	maxFileSize    = 100 * 1024       // 100KB per file
	warnTotalSize  = 10 * 1024 * 1024 // 10MB warning
	errorTotalSize = 50 * 1024 * 1024 // 50MB hard limit
)

// PackageContext walks each path, filtering by extension and size, and returns
// a slice of ContextFile structs ready for upload.
func PackageContext(paths []string) ([]ContextFile, error) {
	var files []ContextFile
	var totalSize int64

	for _, rawPath := range paths {
		expanded := config.ExpandPath(rawPath)
		info, err := os.Stat(expanded)
		if err != nil {
			// Skip paths that don't exist.
			continue
		}

		rootDir := filepath.Base(expanded)

		if !info.IsDir() {
			// Single file
			cf, size, err := readContextFile(expanded, rootDir, filepath.Base(expanded))
			if err != nil {
				continue
			}
			totalSize += size
			files = append(files, cf)
			continue
		}

		// Walk directory
		err = filepath.WalkDir(expanded, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip errors
			}
			if d.IsDir() {
				if excludedDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}

			ext := strings.ToLower(filepath.Ext(d.Name()))
			if !includedExtensions[ext] {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}
			if info.Size() > maxFileSize {
				return nil
			}

			relPath, err := filepath.Rel(expanded, path)
			if err != nil {
				return nil
			}

			cf, size, err := readContextFile(path, rootDir, relPath)
			if err != nil {
				return nil
			}
			totalSize += size
			files = append(files, cf)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking %s: %w", expanded, err)
		}
	}

	if totalSize > errorTotalSize {
		return nil, fmt.Errorf("context too large: %dMB exceeds 50MB limit", totalSize/(1024*1024))
	}
	if totalSize > warnTotalSize {
		fmt.Fprintf(os.Stderr, "Warning: context size is %dMB (>10MB)\n", totalSize/(1024*1024))
	}

	return files, nil
}

func readContextFile(absPath, rootDir, relPath string) (ContextFile, int64, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return ContextFile{}, 0, err
	}
	return ContextFile{
		Path:    filepath.Join(rootDir, relPath),
		Content: string(data),
	}, int64(len(data)), nil
}
