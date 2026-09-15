package fleet

import (
	"bufio"
	"context"
	"strconv"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/remotex"
)

// hardwareProbeScript is a single POSIX /bin/sh script that gathers CPU
// model, logical core count, total system RAM, GPU name+VRAM, and free
// disk space on / in ONE round trip, branching on `uname -s` so the same
// script works unmodified against macOS and Linux (including a WSL2
// guest, which reports Linux). Cores feeds ComputeFleetTotals's numeric
// sum (totals.go) -- it's kept separate from the free-form CPU string
// rather than parsed back out of it.
//
// Every individual lookup is guarded so a missing tool (no lscpu, no
// nvidia-smi, no system_profiler, ...) degrades that one field to empty
// rather than aborting the whole script -- this must never make ProbeOne
// fail just because a box lacks a GPU. GPU detection tries the normal
// nvidia-smi first, then the WSL2 CUDA-passthrough path at
// /usr/lib/wsl/lib/nvidia-smi, which is what makes a Windows box's WSL
// guest (e.g. beast-wsl) report its RTX 4070 -- plain `nvidia-smi` is not
// on PATH there, only the WSL-shimmed binary is. Multiple GPU lines are
// flattened to ';'-separated so the whole result stays one line, matching
// the one-KEY=VALUE-per-line contract parseHardware expects.
const hardwareProbeScript = `
os=$(uname -s 2>/dev/null)
if [ "$os" = "Darwin" ]; then
  cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null)
  cores=$(sysctl -n hw.ncpu 2>/dev/null)
  membytes=$(sysctl -n hw.memsize 2>/dev/null)
  mem=""
  if [ -n "$membytes" ]; then
    mem=$(awk -v b="$membytes" 'BEGIN{printf "%.0fGB", b/1073741824}')
  fi
  disk=$(df -h / 2>/dev/null | awk 'NR==2{print $4" free of "$2}')
  gpu=$(system_profiler SPDisplaysDataType 2>/dev/null | awk -F': ' '/Chipset Model/{print $2; exit}')
else
  cpu=$(lscpu 2>/dev/null | awk -F': +' '/^Model name/{print $2; exit}')
  if [ -z "$cpu" ]; then
    cpu=$(awk -F': +' '/^model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null)
  fi
  cores=$(nproc 2>/dev/null)
  mem=$(free -h 2>/dev/null | awk '/^Mem:/{print $2}')
  disk=$(df -h / 2>/dev/null | awk 'NR==2{print $4" free of "$2}')
  gpu=$(nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null | tr '\n' ';' | sed 's/;$//')
  if [ -z "$gpu" ]; then
    gpu=$(/usr/lib/wsl/lib/nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null | tr '\n' ';' | sed 's/;$//')
  fi
fi
echo "CPU=$cpu"
echo "CORES=$cores"
echo "MEM=$mem"
echo "DISK=$disk"
echo "GPU=$gpu"
`

// probeHardware runs hardwareProbeScript once and parses its output.
// local selects a direct Runner.Run("sh", ...) call for the self device
// (no ssh transport needed -- ProbeOne already trusts self); otherwise it
// goes over remotex.RunRemote to target. Bounded by the same
// probeCallTimeout(ctx) the healthz probe uses, so a slow/wedged box
// can't blow the overall per-device probe budget.
//
// Any failure -- transport error, timeout, empty stdout -- returns nil,
// never an error: hardware info is a best-effort overlay on top of the
// reach/healthz probe, and a device card must never turn red just because
// this extra call didn't come back.
func (p *Prober) probeHardware(ctx context.Context, target string, local bool) *Hardware {
	var (
		res remotex.Result
		err error
	)
	if local {
		res, err = p.Runner.Run(ctx, "sh", []string{"-c", hardwareProbeScript}, nil, probeCallTimeout(ctx))
	} else {
		res, err = remotex.RunRemote(ctx, p.Runner, target, "", []string{"sh", "-c", hardwareProbeScript}, probeCallTimeout(ctx))
	}
	if err != nil || res.TimedOut || len(strings.TrimSpace(string(res.Stdout))) == 0 {
		return nil
	}
	return parseHardware(res.Stdout)
}

// probeOrStaticHardware runs the live combined hardware probe and falls
// back to d's hand-entered config.yaml hardware: override (staticHardware)
// whenever the live probe comes back nil -- a genuinely empty result, a
// transport error, or a timeout. This is only ever called for probe: ssh
// / local devices; tailscale-only and none never attempt a live probe at
// all and go straight to staticHardware (see ProbeOne).
func (p *Prober) probeOrStaticHardware(ctx context.Context, d config.DeviceConfig, target string, local bool) *Hardware {
	if hw := p.probeHardware(ctx, target, local); hw != nil {
		return hw
	}
	return staticHardware(d)
}

// staticHardware converts d's optional config.yaml hardware: override
// into a *Hardware, or nil when none is configured. Used directly for
// devices that can never be live-probed (probe: tailscale-only / none --
// e.g. beast, a Windows host aida never ssh's into) and as the fallback
// path for ssh/local devices above.
func staticHardware(d config.DeviceConfig) *Hardware {
	hc := d.Hardware
	if hc == nil {
		return nil
	}
	return &Hardware{
		CPU:   hc.CPU,
		Cores: hc.Cores,
		Mem:   hc.Mem,
		GPU:   hc.GPU,
		Disk:  hc.Disk,
	}
}

// parseHardware turns hardwareProbeScript's KEY=VALUE lines into a
// Hardware struct. Unknown or missing keys leave that field at its zero
// value ("") rather than erroring -- a partially-populated Hardware (e.g.
// CPU/Mem/Disk but no GPU on a box with no card) is the expected common
// case, not a failure.
func parseHardware(out []byte) *Hardware {
	hw := &Hardware{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "CPU":
			hw.CPU = val
		case "CORES":
			// Best-effort: an empty/garbled value (nproc missing, etc)
			// just leaves Cores at its zero value rather than erroring.
			if n, err := strconv.Atoi(val); err == nil {
				hw.Cores = n
			}
		case "MEM":
			hw.Mem = val
		case "DISK":
			hw.Disk = val
		case "GPU":
			hw.GPU = formatGPU(val)
		}
	}
	return hw
}

// formatGPU reformats hardwareProbeScript's raw GPU field -- one or more
// ';'-separated "name, memory" pairs straight from nvidia-smi's
// --format=csv,noheader, or a single bare name from system_profiler -- into
// a display string like "NVIDIA GeForce RTX 4070 (12282 MiB)", joining
// multiple GPUs with "; ". A shape it doesn't recognize (no comma) is
// passed through verbatim rather than dropped.
func formatGPU(raw string) string {
	if raw == "" {
		return ""
	}
	entries := strings.Split(raw, ";")
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		name, mem, ok := strings.Cut(e, ",")
		if !ok {
			parts = append(parts, e)
			continue
		}
		parts = append(parts, strings.TrimSpace(name)+" ("+strings.TrimSpace(mem)+")")
	}
	return strings.Join(parts, "; ")
}
