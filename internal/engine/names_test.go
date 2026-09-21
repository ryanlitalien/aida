package engine

import "testing"

func TestNormalizeSourceToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"csv-viewer", "csv-viewer"},
		{"CSV-Viewer", "csv-viewer"},
		{"mcp_perforce", "mcp-perforce"},
		{"  Thrive ", "thrive"},
		{"!thrive!", "thrive"},
		{"", ""},
		{"GeminiWatermarkTool", "geminiwatermarktool"}, // camelCase not split here
	}
	for _, c := range cases {
		got := normalizeSourceToken(c.in)
		if got != c.want {
			t.Errorf("normalizeSourceToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTokenizeEntity(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"csv-viewer", []string{"csv", "viewer"}},
		{"mcp-perforce", []string{"mcp", "perforce"}},
		{"thrive game", []string{"thrive", "game"}},
		{"thrive", []string{"thrive"}},
		{"", nil},
	}
	for _, c := range cases {
		got := tokenizeEntity(c.in)
		if len(got) != len(c.want) {
			t.Errorf("tokenizeEntity(%q) length: got %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("tokenizeEntity(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestCompactToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"acme-widgets", "acmewidgets"},
		{"csv-viewer", "csvviewer"},
		{"thrive", "thrive"},
		{"mcp-perforce", "mcpperforce"},
		{"", ""},
	}
	for _, c := range cases {
		got := compactToken(c.in)
		if got != c.want {
			t.Errorf("compactToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCompactMatchScenarios(t *testing.T) {
	// These are the real-world cases: user types various forms, source
	// name is "acme-widgets".
	sourceName := "acme-widgets"
	normName := normalizeSourceToken(sourceName)

	variants := []string{"acmewidgets", "acme_widgets", "acme-widgets", "AcmeWidgets", "Acme_Widgets"}
	for _, v := range variants {
		normEnt := normalizeSourceToken(v)
		if normEnt != normName && compactToken(normEnt) != compactToken(normName) {
			t.Errorf("variant %q: normEnt=%q normName=%q compact(%q)=%q compact(%q)=%q - no match",
				v, normEnt, normName, normEnt, compactToken(normEnt), normName, compactToken(normName))
		}
	}
}

func TestIsGenericNameToken(t *testing.T) {
	for _, tok := range []string{"tool", "tools", "data", "docs", "main", "test", "lib", "app"} {
		if !isGenericNameToken(tok) {
			t.Errorf("isGenericNameToken(%q) = false, want true", tok)
		}
	}
	for _, tok := range []string{"viewer", "perforce", "thrive", "openscreen", "watermark"} {
		if isGenericNameToken(tok) {
			t.Errorf("isGenericNameToken(%q) = true, want false", tok)
		}
	}
}
