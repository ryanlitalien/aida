package llm

import (
	"context"
	"strings"
	"testing"
)

func TestStripGoogleModelPrefix(t *testing.T) {
	cases := map[string]string{
		"gemini-3.7-flash":        "gemini-3.7-flash",
		"google:gemini-3.7-flash": "gemini-3.7-flash",
		"gemini:gemini-3.7-flash": "gemini-3.7-flash",
		"GOOGLE:gemini-3.7-flash": "gemini-3.7-flash",
		"openai:gpt-4o":           "openai:gpt-4o", // not ours to strip
		"":                        "",
	}
	for in, want := range cases {
		if got := stripGoogleModelPrefix(in); got != want {
			t.Errorf("stripGoogleModelPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveProviderGemini(t *testing.T) {
	for _, model := range []string{"gemini-3.7-flash", "google:gemini-3.7-flash", "gemini:foo"} {
		if got := ResolveProvider(model); got != "google" {
			t.Errorf("ResolveProvider(%q) = %q, want google", model, got)
		}
	}
}

// A Gemini model must reach GoogleProvider, not silently fall through to
// Anthropic - the bug this provider was added to fix.
func TestNewClientRoutesGeminiToGoogle(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	c := NewClient("anthropic-key", "gemini-3.7-flash", false)
	if got := c.provider.Name(); got != "google" {
		t.Errorf("provider = %q, want google", got)
	}
}

// --offline must never reach a cloud provider, no matter what model name it
// is handed. A cloud-resolving model (e.g. model.fallback configured to
// gemini-3.7-flash - the pre-fix aida init default) previously reached
// GoogleProvider with no credential and failed with "google: no credential"
// instead of running locally. Local names must still land on Ollama too.
func TestOfflineClientResolvesFallbackProvider(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	cases := map[string]string{
		"gemini-3.7-flash": "ollama",
		"qwen3:8b":         "ollama",
		"llama3.1":         "ollama",
		"some-unknown":     "ollama",
	}
	for model, want := range cases {
		c := NewClient("anthropic-key", model, true)
		if got := c.provider.Name(); got != want {
			t.Errorf("offline NewClient(%q) provider = %q, want %q", model, got, want)
		}
	}
}

// --offline set + model.fallback configured to a cloud provider -> the
// Ollama default model is chosen instead, never the cloud model name.
func TestOfflineClientIgnoresCloudFallbackModel(t *testing.T) {
	c := NewClient("anthropic-key", "gemini-3.7-flash", true)
	if got := c.provider.Name(); got != "ollama" {
		t.Errorf("offline provider = %q, want ollama", got)
	}
	if got := c.Model(); got != DefaultOllamaModel {
		t.Errorf("offline model = %q, want %q (the Ollama default)", got, DefaultOllamaModel)
	}
}

// ResolveOfflineModel backs the same guarantee at the config-resolution
// layer used by aida's query/investigate/session commands: a cloud
// fallback model is ignored in favor of the Ollama default, an explicit
// model.offline override always wins, and a genuinely local candidate
// (or override) is kept as-is.
func TestResolveOfflineModel(t *testing.T) {
	cases := []struct {
		name      string
		candidate string
		override  string
		want      string
	}{
		{"cloud fallback ignored", "gemini-3.7-flash", "", DefaultOllamaModel},
		{"empty candidate defaults", "", "", DefaultOllamaModel},
		{"local candidate kept", "qwen3:8b", "", "qwen3:8b"},
		{"override always wins over cloud", "gemini-3.7-flash", "llama3.1", "llama3.1"},
		{"override always wins over local", "qwen3:8b", "llama3.1", "llama3.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveOfflineModel(tc.candidate, tc.override); got != tc.want {
				t.Errorf("ResolveOfflineModel(%q, %q) = %q, want %q", tc.candidate, tc.override, got, tc.want)
			}
		})
	}
}

func TestGoogleProviderRequiresCredential(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	p := NewGoogleProvider("")
	_, err := p.Complete(context.Background(), Request{Model: "gemini-3.7-flash"})
	if err == nil {
		t.Fatal("expected an error when no credential is set")
	}
	if !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Errorf("error should name the env var, got: %v", err)
	}
}

func TestGeminiPricingLookup(t *testing.T) {
	p := LookupPricing("gemini-3.7-flash")
	if p.InputPerM != 0.75 || p.OutputPerM != 3.75 {
		t.Errorf("gemini-3.7-flash pricing = %+v, want 0.75/3.75", p)
	}
	if got := LookupPricing("google:gemini-3.7-flash"); got != p {
		t.Errorf("prefixed lookup = %+v, want %+v", got, p)
	}
}
