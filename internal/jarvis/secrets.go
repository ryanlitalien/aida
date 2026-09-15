package jarvis

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
)

// SecretsFile is the path to the optional env file aida loads into the
// process environment at startup. Lines look like `KEY=value`. Comments
// (lines starting with `#`) and blank lines are ignored. The file MUST be
// 0600 or aida refuses to load it (prevents accidental world-readable
// secrets after a chmod botch).
//
// Today this exists so callers can avoid the per-process `op read`
// round-trip (and its Touch ID prompt) for ELEVENLABS_API_KEY without
// having to bake the key into a shell rc. Generalizes to any
// jarvis-owned secret in the future.
func SecretsFile() string {
	return filepath.Join(config.Dir(), "secrets.env")
}

// LoadSecretsEnv reads SecretsFile() and exports each KEY=value pair via
// os.Setenv. Missing file → no-op. Mode != 0600 → loud warning, skip
// (we'd rather the user notice than silently leak). Existing env vars
// win - the file is a fallback, not an override, so shell-level exports
// can still take precedence.
func LoadSecretsEnv() error {
	path := SecretsFile()
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	// 0077 mask isolates "group + world" bits; any of them set → refuse.
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "jarvis: refusing to load %s - mode is %o, expected 600\n", path, info.Mode().Perm())
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		// Strip optional surrounding quotes so the file format matches
		// what `op run --env-file` accepts.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, val)
	}
	return scan.Err()
}
