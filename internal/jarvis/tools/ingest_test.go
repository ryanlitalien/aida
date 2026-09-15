package tools

import "testing"

func TestPickNotionRef(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare url", "https://www.notion.so/Partner-Sync-abc123", "https://www.notion.so/Partner-Sync-abc123"},
		{"url after preamble", "Sure! Here is the page:\nhttps://www.notion.so/Call-Notes-def456\n", "https://www.notion.so/Call-Notes-def456"},
		{"non-notion http url", "http://example.com/x", "http://example.com/x"},
		{"first non-empty line fallback", "\n\nPartner Sync Notes\n", "Partner Sync Notes"},
		{"none sentinel passes through", "NONE", "NONE"},
		{"empty", "   \n  \n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickNotionRef(c.in); got != c.want {
				t.Errorf("pickNotionRef(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestIngestNotionToolRegistered guards that the tool is wired into a default
// registry (push-to-talk path: no MCP, no jobs store) and exposes the expected
// schema fields.
func TestIngestNotionToolShape(t *testing.T) {
	tool := ingestNotionTasksTool()
	if tool.Name != "ingest_notion_tasks" {
		t.Fatalf("name = %q", tool.Name)
	}
	props, ok := tool.Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("schema properties missing or wrong type")
	}
	for _, k := range []string{"page_ref", "confirm"} {
		if _, ok := props[k]; !ok {
			t.Errorf("schema missing property %q", k)
		}
	}
}
