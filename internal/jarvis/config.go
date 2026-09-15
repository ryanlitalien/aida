package jarvis

import (
	"os"
	"path/filepath"

	"github.com/ryanlitalien/aida/internal/dispatch"
)

// Config controls Jarvis runtime behavior. Loaded from
// ~/.aida/jarvis.yaml when present, else falls back to defaults.
type Config struct {
	Enabled bool `yaml:"enabled"`

	// Models directory; defaults to ~/.aida/jarvis/models
	ModelsDir string `yaml:"models_dir"`

	// TTS
	PiperBin      string `yaml:"piper_bin"`       // path to piper binary
	PiperModel    string `yaml:"piper_model"`     // ONNX model filename inside ModelsDir
	EspeakDataDir string `yaml:"espeak_data_dir"` // path to espeak-ng-data dir for piper

	// Optional ElevenLabs override. If both VoiceID is set and a key can
	// be resolved (env var or 1Password CLI), Jarvis uses ElevenLabs.
	// Otherwise it falls back to local Piper. The env var
	// ELEVENLABS_VOICE_ID overrides ElevenLabsVoiceID at runtime.
	ElevenLabsVoiceID    string  `yaml:"elevenlabs_voice_id"`   // ElevenLabs voice id
	ElevenLabsModelID    string  `yaml:"elevenlabs_model_id"`   // default: eleven_multilingual_v2
	ElevenLabsOpRef      string  `yaml:"elevenlabs_op_ref"`     // e.g. op://Aida/ElevenLabs API - jarvis/credential
	ElevenLabsStability  float64 `yaml:"elevenlabs_stability"`  // 0-1; default 0.5
	ElevenLabsSimilarity float64 `yaml:"elevenlabs_similarity"` // 0-1; default 0.75
	ElevenLabsStyle      float64 `yaml:"elevenlabs_style"`      // 0-1; default 0
	ElevenLabsSpeed      float64 `yaml:"elevenlabs_speed"`      // 0.7-1.2; default 1.0

	// LLM
	Model     string  `yaml:"model"` // anthropic model id, e.g. claude-haiku-4-5
	MaxTokens int     `yaml:"max_tokens"`
	Temp      float64 `yaml:"temperature"`

	// Voice / audio
	WakeWord string `yaml:"wake_word"` // "ok jarvis"
	Greeting string `yaml:"greeting"`  // spoken on daemon start

	// Volume is the playback gain multiplier for this persona's voice,
	// on top of system volume. 0 falls back to the tts package default.
	// Per-persona because the two ElevenLabs voices differ in inherent
	// loudness: Jarvis reads quieter than Aida at the same gain.
	Volume float64 `yaml:"volume"`

	// Persona is the assistant's display name, substituted into the system
	// prompt and spoken identity. Defaults to "Jarvis". A second config with
	// a different Persona + voice + WakePattern is how the cosmetic-twin
	// wake word ("ok aida") is wired.
	Persona string `yaml:"persona"`

	// WakePattern is the raw regex alternation matched after the wake prefix
	// (ok/okay/hey/good morning…). Defaults to "jarvis". Aida uses a
	// whisper-tolerant alternation ("aida|ada|ayda|ida") because tiny.en
	// mangles the AY-duh vowel.
	WakePattern string `yaml:"wake_pattern"`

	// WakePrefix is the regex alternation of accepted wake prefixes. Empty
	// uses the full default set (ok/okay/hey/good morning…). Aida narrows it
	// to just "hey" - one crisp phrase reads more reliably than "ok aida".
	WakePrefix string `yaml:"wake_prefix"`

	// Tone overrides the default "concise, dry, and lightly witty" descriptor
	// with a persona-specific adjective phrase (e.g. "concise, warm, and
	// personable"). Empty keeps Jarvis's dry default.
	Tone string `yaml:"tone"`

	// Personalization - injected into the system prompt so the LLM knows
	// the user's default location for weather, time-zone, etc., without
	// having to ask back over voice.
	Home string `yaml:"home"` // e.g. "Boston, MA"

	// Demo / test surface
	TextOnly bool `yaml:"text_only"` // skip audio I/O, print transcript instead

	// Dispatcher, when non-nil, upgrades this persona from a plain assistant
	// into Aida the chief-of-staff: her tool registry gains ask_agent +
	// roster_list and her prompt gains a dispatcher block. Nil for Jarvis (the
	// default), so his registry and prompt stay byte-identical. Set at runtime
	// by `aida serve`, never serialized.
	Dispatcher *dispatch.Dispatcher `yaml:"-"`

	// AutoSync mirrors the top-level aida config's Brain.AutoSync. It is
	// not itself part of jarvis.yaml - Jarvis has no brain-sync settings of
	// its own - but the caller (aida serve / aida jarvis ...) copies it in
	// from the loaded aida config so the tasks_add voice tool can pull the
	// brain repo before allocating a task id, same as every other add path.
	// Never serialized.
	AutoSync bool `yaml:"-"`
}

// DefaultConfig returns a working baseline.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	binDir := filepath.Join(home, ".aida", "jarvis", "bin")
	return Config{
		Enabled:       true,
		ModelsDir:     filepath.Join(home, ".aida", "jarvis", "models"),
		PiperBin:      filepath.Join(binDir, "piper"),
		PiperModel:    "jarvis-high.onnx",
		EspeakDataDir: filepath.Join(binDir, "espeak-ng-data"),

		ElevenLabsVoiceID:    "mfGn240ErL2HMopgBro1", // custom Jarvis IVC (MCU lines)
		ElevenLabsModelID:    "eleven_multilingual_v2",
		ElevenLabsOpRef:      "op://Aida/ElevenLabs API - jarvis/credential",
		ElevenLabsStability:  1.0,
		ElevenLabsSimilarity: 1.0,
		ElevenLabsStyle:      0.05,
		ElevenLabsSpeed:      0.85,
		Model:                "claude-haiku-4-5",
		MaxTokens:            512,
		Temp:                 0.7,
		WakeWord:             "ok jarvis",
		Greeting:             "Good morning, sir.",
		Volume:               1.43,
		Home:                 "Boston, MA",
		Persona:              "Jarvis",
		WakePattern:          "jarvis",
	}
}

// ModelPath joins ModelsDir with the configured Piper model filename.
func (c Config) ModelPath() string {
	return filepath.Join(c.ModelsDir, c.PiperModel)
}
