package sources

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPickIDColumn_PrefersStringOverNumericAndDate(t *testing.T) {
	headers := []string{"month", "bucket", "total_spent"}
	rows := []map[string]string{
		{"month": "2026-04", "bucket": "ButterStack", "total_spent": "316.19"},
		{"month": "2026-04", "bucket": "Camp Butz", "total_spent": "3098.61"},
		{"month": "2026-04", "bucket": "Personal", "total_spent": "41289.50"},
	}
	got := pickIDColumn(headers, rows)
	if got != "bucket" {
		t.Errorf("pickIDColumn() = %q, want %q", got, "bucket")
	}
}

func TestPickIDColumn_FallsBackToDate(t *testing.T) {
	headers := []string{"date", "amount"}
	rows := []map[string]string{
		{"date": "2026-03-15", "amount": "500.00"},
		{"date": "2026-03-16", "amount": "42.50"},
	}
	got := pickIDColumn(headers, rows)
	// date looks like a date-only string, amount is numeric.
	// No string column → falls back to first non-empty = "date".
	if got != "date" {
		t.Errorf("pickIDColumn() = %q, want %q", got, "date")
	}
}

func TestPickIDColumn_Empty(t *testing.T) {
	got := pickIDColumn(nil, nil)
	if got != "" {
		t.Errorf("pickIDColumn() = %q, want empty", got)
	}
}

func TestDeriveRowIDFromColumn(t *testing.T) {
	row := map[string]string{"bucket": "Camp Butz", "amount": "3098.61"}
	got := deriveRowIDFromColumn(0, row, "bucket")
	if got != "camp-butz" {
		t.Errorf("deriveRowIDFromColumn() = %q, want %q", got, "camp-butz")
	}
}

func TestDeriveRowIDFromColumn_Fallback(t *testing.T) {
	row := map[string]string{"amount": "42.50"}
	got := deriveRowIDFromColumn(3, row, "")
	if got != "row-3" {
		t.Errorf("deriveRowIDFromColumn() = %q, want %q", got, "row-3")
	}
}

func TestDeriveRowIDFromColumn_MissingValue(t *testing.T) {
	row := map[string]string{"bucket": "", "amount": "42.50"}
	got := deriveRowIDFromColumn(2, row, "bucket")
	if got != "row-2" {
		t.Errorf("deriveRowIDFromColumn() = %q, want %q", got, "row-2")
	}
}

func TestLooksNumeric(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"42", true},
		{"3.14", true},
		{"-100", true},
		{"1,234.56", true},
		{"0", true},
		{"ButterStack", false},
		{"2026-04", false},
		{"", false},
		{"12abc", false},
	}
	for _, tc := range cases {
		got := looksNumeric(tc.input)
		if got != tc.want {
			t.Errorf("looksNumeric(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestLooksDateOnly(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"2026-04", true},
		{"2026-03-15", true},
		{"2026-03-15T10:00:00Z", true},
		{"ButterStack", false},
		{"42.50", false},
		{"", false},
		{"Camp Butz", false},
	}
	for _, tc := range cases {
		got := looksDateOnly(tc.input)
		if got != tc.want {
			t.Errorf("looksDateOnly(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestPickIDColumnFromMaps(t *testing.T) {
	keys := []string{"CATEGORY", "AMOUNT", "CREATED_AT"}
	rows := []map[string]interface{}{
		{"CATEGORY": "Dining Out", "AMOUNT": 42.5, "CREATED_AT": "2026-03-15"},
		{"CATEGORY": "Groceries", "AMOUNT": 87.32, "CREATED_AT": "2026-03-16"},
	}
	got := pickIDColumnFromMaps(keys, rows)
	if got != "CATEGORY" {
		t.Errorf("pickIDColumnFromMaps() = %q, want %q", got, "CATEGORY")
	}
}

func TestSanitizeRowID_Truncation(t *testing.T) {
	long := "This Is A Very Long Category Name That Should Be Truncated To Keep It Short"
	got := sanitizeRowID(long)
	if len(got) > 40 {
		t.Errorf("sanitizeRowID() len = %d (> 40): %q", len(got), got)
	}
}

func TestSanitizeRowID_SpecialChars(t *testing.T) {
	got := sanitizeRowID("Food & Dining / Restaurants")
	if got != "food-dining-restaurants" {
		t.Errorf("sanitizeRowID() = %q, want %q", got, "food-dining-restaurants")
	}
}

func TestEnsureUniqueIDs(t *testing.T) {
	artifacts := []Artifact{
		{ID: "groceries"},
		{ID: "groceries"},
		{ID: "dining"},
		{ID: "groceries"},
		{ID: "dining"},
	}
	ensureUniqueIDs(artifacts)

	want := []string{"groceries", "groceries-2", "dining", "groceries-3", "dining-2"}
	for i, a := range artifacts {
		if a.ID != want[i] {
			t.Errorf("artifacts[%d].ID = %q, want %q", i, a.ID, want[i])
		}
	}
}

func TestEnsureUniqueIDs_NoDuplicates(t *testing.T) {
	artifacts := []Artifact{
		{ID: "groceries"},
		{ID: "dining"},
		{ID: "transport"},
	}
	ensureUniqueIDs(artifacts)

	want := []string{"groceries", "dining", "transport"}
	for i, a := range artifacts {
		if a.ID != want[i] {
			t.Errorf("artifacts[%d].ID = %q, want %q", i, a.ID, want[i])
		}
	}
}

func TestJSONRowID_GitHubPRShape(t *testing.T) {
	row := map[string]interface{}{
		"repository": map[string]interface{}{"name": "butter_stack"},
		"number":     float64(3476),
		"title":      "Bump three from 0.170.0 to 0.180.0",
		"state":      "OPEN",
	}
	got := jsonRowID(row, "", 0)
	if got != "butter-stack-3476" {
		t.Errorf("jsonRowID() = %q, want %q", got, "butter-stack-3476")
	}
}

func TestJSONRowID_GitHubSearchNameWithOwner(t *testing.T) {
	row := map[string]interface{}{
		"repository": map[string]interface{}{"nameWithOwner": "ryanlitalien/aida"},
		"number":     float64(34),
		"title":      "orchestrator: add streaming support",
	}
	got := jsonRowID(row, "", 0)
	// sanitizeRowID lowercases and replaces "/" with "-", truncates at 40.
	if got != "ryanlitalien-aida-34" {
		t.Errorf("jsonRowID() = %q, want %q", got, "ryanlitalien-aida-34")
	}
}

func TestJSONRowID_PrefersTopLevelID(t *testing.T) {
	row := map[string]interface{}{
		"id":   "PR_abc123",
		"name": "something-else",
	}
	got := jsonRowID(row, "", 0)
	if got != "pr-abc123" {
		t.Errorf("jsonRowID() = %q, want %q", got, "pr-abc123")
	}
}

func TestJSONRowID_NumberAloneIsUsed(t *testing.T) {
	// number without a repository object still beats item-N.
	row := map[string]interface{}{
		"number": float64(42),
		"state":  "OPEN",
	}
	got := jsonRowID(row, "", 0)
	if got != "42" {
		t.Errorf("jsonRowID() = %q, want %q", got, "42")
	}
}

func TestJSONRowID_FallsBackToPickIDColumn(t *testing.T) {
	// No GitHub shape, no id/number/name/title - should use the picker hint.
	row := map[string]interface{}{
		"color": "periwinkle",
		"count": float64(7),
	}
	got := jsonRowID(row, "color", 2)
	if got != "periwinkle" {
		t.Errorf("jsonRowID() = %q, want %q", got, "periwinkle")
	}
}

func TestJSONRowID_LastResortItemN(t *testing.T) {
	// Nothing useful anywhere → item-N.
	row := map[string]interface{}{
		"count": float64(7),
	}
	got := jsonRowID(row, "", 3)
	if got != "item-3" {
		t.Errorf("jsonRowID() = %q, want %q", got, "item-3")
	}
}

func TestExtractRepoName(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"bare string", "butter_stack", "butter_stack"},
		{"nested name", map[string]interface{}{"name": "butter_stack"}, "butter_stack"},
		{"nameWithOwner", map[string]interface{}{"nameWithOwner": "org/repo"}, "org/repo"},
		{"full_name", map[string]interface{}{"full_name": "org/repo"}, "org/repo"},
		{"prefers nameWithOwner", map[string]interface{}{"name": "repo", "nameWithOwner": "org/repo"}, "org/repo"},
		{"nil", nil, ""},
		{"unknown shape", 42, ""},
		{"empty map", map[string]interface{}{}, ""},
	}
	for _, tc := range cases {
		got := extractRepoName(tc.in)
		if got != tc.want {
			t.Errorf("extractRepoName(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStringFromJSON(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},
		{"", ""},
		{"  hello  ", "hello"},
		{float64(42), "42"},
		{float64(3.14), "3.14"},
		{true, "true"},
		{map[string]interface{}{}, ""},
	}
	for _, tc := range cases {
		got := stringFromJSON(tc.in)
		if got != tc.want {
			t.Errorf("stringFromJSON(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Integration-style test mimicking `gh search prs --json repository,number,title,state`
// output flowing through ExecAdapter.ParseOutput → jsonRowID → ensureUniqueIDs.
// Confirms that two PRs in the same repo produce two distinct, meaningful IDs.
func TestJSONRowID_FullFlowGitHubPRs(t *testing.T) {
	rows := []map[string]interface{}{
		{
			"repository": map[string]interface{}{"name": "butter_stack"},
			"number":     float64(3476),
			"title":      "Bump web-console",
		},
		{
			"repository": map[string]interface{}{"name": "butter_stack"},
			"number":     float64(3480),
			"title":      "Bump @axe-core/playwright to 4.10.2",
		},
		{
			"repository": map[string]interface{}{"name": "butter_stack"},
			"number":     float64(3481),
			"title":      "Bump @axe-core/playwright to 4.10.3",
		},
	}
	var artifacts []Artifact
	for i, row := range rows {
		artifacts = append(artifacts, Artifact{ID: jsonRowID(row, "", i)})
	}
	ensureUniqueIDs(artifacts)
	want := []string{"butter-stack-3476", "butter-stack-3480", "butter-stack-3481"}
	for i, a := range artifacts {
		if a.ID != want[i] {
			t.Errorf("artifacts[%d].ID = %q, want %q", i, a.ID, want[i])
		}
	}
}

func TestPickIDColumn_PrefersMostDistinct(t *testing.T) {
	headers := []string{"bucket", "category", "total_expenses"}
	rows := []map[string]string{
		{"bucket": "Personal", "category": "Housing", "total_expenses": "2182.27"},
		{"bucket": "Personal", "category": "Travel", "total_expenses": "801.38"},
		{"bucket": "Personal", "category": "Food", "total_expenses": "212.88"},
		{"bucket": "Personal", "category": "Utilities", "total_expenses": "114.11"},
		{"bucket": "ButterStack", "category": "Technology", "total_expenses": "118.53"},
		{"bucket": "Camp Butz", "category": "Housing", "total_expenses": "600.00"},
	}
	got := pickIDColumn(headers, rows)
	// category has 5 distinct values, bucket has 3 → should pick category
	if got != "category" {
		t.Errorf("pickIDColumn() = %q, want %q", got, "category")
	}
}

// TestSourceResult_FromContextDoc verifies the FromContextDoc flag defaults
// to false (and is omitted from JSON) for an ordinary result, and round-trips
// as true for a context-file-fallback result -- callers downstream (the
// synthesizer, the verifier) rely on this to tell "documentation ABOUT a
// source" apart from "data FROM it".
func TestSourceResult_FromContextDoc(t *testing.T) {
	ordinary := SourceResult{Source: "test-source", Status: "success"}
	if ordinary.FromContextDoc {
		t.Error("expected FromContextDoc to default to false")
	}
	data, err := json.Marshal(ordinary)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "from_context_doc") {
		t.Errorf("expected from_context_doc omitted when false, got: %s", data)
	}

	fallback := SourceResult{Source: "test-source", Status: "success", FromContextDoc: true}
	data, err = json.Marshal(fallback)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"from_context_doc":true`) {
		t.Errorf("expected from_context_doc:true in JSON, got: %s", data)
	}
}

// Integration-style test: mimics the full flow for a finance query.
func TestFullFlow_FinanceBuckets(t *testing.T) {
	headers := []string{"month", "bucket", "total_spent"}
	rows := []map[string]string{
		{"month": "2026-04", "bucket": "ButterStack", "total_spent": "316.19"},
		{"month": "2026-04", "bucket": "Camp Butz", "total_spent": "3098.61"},
		{"month": "2026-04", "bucket": "Personal", "total_spent": "41289.50"},
	}

	idCol := pickIDColumn(headers, rows)
	var artifacts []Artifact
	for i, row := range rows {
		artifacts = append(artifacts, Artifact{
			ID: deriveRowIDFromColumn(i, row, idCol),
		})
	}
	ensureUniqueIDs(artifacts)

	want := []string{"butterstack", "camp-butz", "personal"}
	for i, a := range artifacts {
		if a.ID != want[i] {
			t.Errorf("artifacts[%d].ID = %q, want %q", i, a.ID, want[i])
		}
	}
}
