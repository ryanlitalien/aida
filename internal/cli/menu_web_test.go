package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const menuFixtureYAML = `
anchor:
  kid_weekend_friday: 2026-09-04

people:
  ryan: {}
  kids:
    - name: Jack
    - name: Mia
      rules:
        - Does not eat chicken or eggs Ryan cooks.
        - Nuggets stay in the freezer as her fallback.
    - name: Sam
      rules:
        - No tree nuts, ever.

breakfast:
  name: Eggs, spinach, avocado
  items:
    - {food: Eggs, qty: 3 large, cal: 216, p: 18, c: 1, f: 15}
    - {food: Spinach, qty: 60g, cal: 14, p: 2, c: 2, f: 0}
    - {food: Avocado, qty: 75g, cal: 120, p: 1, c: 6, f: 11}
  totals: {cal: 350, p: 21, c: 9, f: 26}
  recipe:
    - Scramble the eggs.
    - Plate with avocado.

lunch:
  weekday:
    name: Chicken bowl
    items:
      - {food: Chicken breast, qty: 200g, cal: 330, p: 62, c: 0, f: 7}
    totals: {cal: 330, p: 62, c: 0, f: 7}
  weekend:
    name: Tuna and avocado plate
    items:
      - {food: Tuna in water, qty: 1 can, cal: 150, p: 33, c: 0, f: 1}
    totals: {cal: 150, p: 33, c: 0, f: 1}

snack:
  name: Shake and almonds
  items:
    - {food: Whey protein, qty: 1 scoop, cal: 120, p: 24, c: 3, f: 1}
  totals: {cal: 120, p: 24, c: 3, f: 1}

dinners:
  sun:
    name: Sheet-pan salmon and broccoli
    kids: false
    recipe:
      - Oven 400F.
      - Roast the salmon and broccoli.
  mon:
    name: Chicken thighs and roasted cauliflower
    kids: false
  tue:
    name: Beef tacos
    kids: true
    kid_version: Same taco beef in flour tortillas.
  wed:
    name: Baked cod, zoodles, marinara
    kids: false
  thu:
    name: Beef meatballs and marinara
    kids: true
    kid_version: Same meatballs over real pasta with parmesan.
    prep_ahead:
      - {when: sunday-cook, step: "Meatballs are made and frozen at the Sunday cook."}
      - {when: night-before, step: "Wednesday night: move the meatballs from freezer to fridge."}
  fri:
    name: Burgers
    kids: true
    kids_weeks: [A]
    kid_version: Patties on buns with cheddar, plus oven fries.
    prep_ahead:
      - {when: morning-of, step: "Friday morning: move patties from freezer to fridge."}
      - {when: before-dinner, step: "30 min out: pull the cheddar from the fridge."}
  sat:
    A:
      name: Beef stir fry
      kids: true
      kid_version: Same stir fry over white rice.
    B:
      name: Steak and asparagus (long-ride night)
      kids: false

kids_weekend_food:
  - Chicken nuggets
  - Quesadilla kit
  - Fruit, cereal, oat milk, bagels or waffles

sunday_cook:
  steps:
    - "Step one."
    - "Step two."
  thursday_rule: Move Thu and Fri portions from freezer to fridge.

grocery:
  budget_usd: 100
  weekly:
    proteins:
      - {item: Ground beef, qty: 1 kg, for: tacos, est_usd: 10.00}
      - {item: Sirloin, qty: 250g, for: Saturday, est_usd: 6.00, week_a_extra_usd: 6.00}
    produce:
      - {item: Asparagus, qty: 1 bunch, for: Saturday B, est_usd: 3.00, week: B}
      - {item: Spinach, qty: 300g, for: breakfast, est_usd: 4.00}
    kids:
      - {item: Burger buns, qty: 1 pack, for: Friday A, est_usd: 2.50, week: A}
      - {item: "Chicken nuggets, frozen", qty: 1 bag, for: fallback, est_usd: 7.00, as_needed: true}
  trip_items:
    - {item: Bananas, note: pre-workout}
  monthly_pantry:
    - {item: Olive oil, qty: 1 L, est_usd: 9.00}
    - {item: Coconut aminos, qty: 1 bottle, mb_or_wf: true, est_usd: 7.00}
`

func newMenuTestMux(t *testing.T, yamlContent string) *http.ServeMux {
	t.Helper()
	return newMenuTestMuxLAN(t, yamlContent, false)
}

// newMenuTestMuxLAN is newMenuTestMux with an explicit isLAN, for the tests
// that need to assert on the "lan" field of the JSON response.
func newMenuTestMuxLAN(t *testing.T, yamlContent string, isLAN bool) *http.ServeMux {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "menu.yaml")
	if err := writeFile(path, yamlContent); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	mux := http.NewServeMux()
	registerMenuWebRoutes(mux, path, isLAN)
	return mux
}

// writeFile is a tiny local helper (os.WriteFile wrapper).
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func getMenuJSON(t *testing.T, mux *http.ServeMux, url string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response body not valid JSON for %s: %v (body=%q)", url, err, rec.Body.String())
		}
	}
	return rec.Code, body
}

// TestMenuLANFlagPropagation asserts that /api/menu's "lan" field reflects
// the isLAN param registerMenuWebRoutes was registered with -- the page
// uses this to hide its nav (whose other links 404 on the LAN-only mux).
func TestMenuLANFlagPropagation(t *testing.T) {
	loopbackMux := newMenuTestMuxLAN(t, menuFixtureYAML, false)
	_, loopbackBody := getMenuJSON(t, loopbackMux, "/api/menu?date=2026-08-30")
	if lan, _ := loopbackBody["lan"].(bool); lan != false {
		t.Errorf("loopback mux: lan = %v, want false", loopbackBody["lan"])
	}

	lanMux := newMenuTestMuxLAN(t, menuFixtureYAML, true)
	_, lanBody := getMenuJSON(t, lanMux, "/api/menu?date=2026-08-30")
	if lan, _ := lanBody["lan"].(bool); lan != true {
		t.Errorf("LAN mux: lan = %v, want true", lanBody["lan"])
	}
}

func TestMenuWeekLabelAcrossReferenceDates(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	cases := []struct {
		date string
		want string
	}{
		{"2026-08-30", "A"}, // Sunday, week Aug30-Sep5, Friday=Sep4, diff=0
		{"2026-09-06", "B"}, // Sunday, week Sep6-Sep12, Friday=Sep11, diff=7
		{"2026-09-13", "A"}, // Sunday, week Sep13-Sep19, Friday=Sep18, diff=14
		{"2026-09-20", "B"}, // Sunday, week Sep20-Sep26, Friday=Sep25, diff=21
		{"2026-09-09", "B"}, // mid-week Wednesday, same week as 2026-09-06
	}

	for _, tc := range cases {
		code, body := getMenuJSON(t, mux, "/api/menu?date="+tc.date)
		if code != http.StatusOK {
			t.Fatalf("date=%s: status = %d, want 200 (body=%v)", tc.date, code, body)
		}
		got, _ := body["week"].(string)
		if got != tc.want {
			t.Errorf("date=%s: week = %q, want %q", tc.date, got, tc.want)
		}
	}
}

func TestMenuSaturdayPicksWeekSubMap(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	// Week A: 2026-08-30.
	_, bodyA := getMenuJSON(t, mux, "/api/menu?date=2026-08-30")
	satA := findDay(t, bodyA, "sat")
	dinnerA, _ := satA["dinner"].(map[string]any)
	if name, _ := dinnerA["name"].(string); name != "Beef stir fry" {
		t.Errorf("week A sat dinner name = %q, want %q", name, "Beef stir fry")
	}
	if kids, _ := dinnerA["kids"].(bool); kids != true {
		t.Errorf("week A sat dinner kids = %v, want true", kids)
	}

	// Week B: 2026-09-06.
	_, bodyB := getMenuJSON(t, mux, "/api/menu?date=2026-09-06")
	satB := findDay(t, bodyB, "sat")
	dinnerB, _ := satB["dinner"].(map[string]any)
	if name, _ := dinnerB["name"].(string); name != "Steak and asparagus (long-ride night)" {
		t.Errorf("week B sat dinner name = %q, want %q", name, "Steak and asparagus (long-ride night)")
	}
	if kids, _ := dinnerB["kids"].(bool); kids != false {
		t.Errorf("week B sat dinner kids = %v, want false", kids)
	}
}

func TestMenuFridayKidsWeeksFilter(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, bodyA := getMenuJSON(t, mux, "/api/menu?date=2026-08-30") // week A
	friA := findDay(t, bodyA, "fri")
	dinnerA, _ := friA["dinner"].(map[string]any)
	if kids, _ := dinnerA["kids"].(bool); kids != true {
		t.Errorf("week A friday kids = %v, want true", kids)
	}

	_, bodyB := getMenuJSON(t, mux, "/api/menu?date=2026-09-06") // week B
	friB := findDay(t, bodyB, "fri")
	dinnerB, _ := friB["dinner"].(map[string]any)
	if kids, _ := dinnerB["kids"].(bool); kids != false {
		t.Errorf("week B friday kids = %v, want false", kids)
	}
}

func TestMenuTodayHighlighting(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-09-09") // Wednesday
	days, _ := body["days"].([]any)
	if len(days) != 7 {
		t.Fatalf("days length = %d, want 7", len(days))
	}
	todayCount := 0
	var todayDay string
	for _, d := range days {
		dm, _ := d.(map[string]any)
		if isToday, _ := dm["is_today"].(bool); isToday {
			todayCount++
			todayDay, _ = dm["day"].(string)
		}
	}
	if todayCount != 1 {
		t.Fatalf("exactly one is_today expected, got %d", todayCount)
	}
	if todayDay != "wed" {
		t.Errorf("today day = %q, want %q", todayDay, "wed")
	}
	if today, _ := body["today"].(string); today != "wed" {
		t.Errorf("top-level today = %q, want %q", today, "wed")
	}
}

func TestMenuMissingFileReturnsCleanError(t *testing.T) {
	mux := http.NewServeMux()
	registerMenuWebRoutes(mux, "/nonexistent/path/menu.yaml", false)

	req := httptest.NewRequest("GET", "/api/menu", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not valid JSON: %v (body=%q)", err, rec.Body.String())
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("expected an %q key in error body, got %v", "error", body)
	}
}

func TestMenuMalformedDateReturns400(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	req := httptest.NewRequest("GET", "/api/menu?date=not-a-date", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestMenuMuxIsolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "menu.yaml")
	if err := writeFile(path, menuFixtureYAML); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	mux := http.NewServeMux()
	registerMenuWebRoutes(mux, path, false)

	// A route this mux never registered must 404, proving a LAN-only
	// listener that mounts only menu routes serves nothing else.
	req := httptest.NewRequest("GET", "/tasks", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /tasks on menu-only mux: status = %d, want 404", rec.Code)
	}

	// Wrong method on a registered path must 405 thanks to the
	// method-prefixed pattern (GET /menu, GET /api/menu).
	req = httptest.NewRequest("POST", "/menu", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /menu: status = %d, want 405", rec.Code)
	}

	req = httptest.NewRequest("POST", "/api/menu", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/menu: status = %d, want 405", rec.Code)
	}
}

func TestMenuHTMLRoutes(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	for _, path := range []string{"/menu", "/menu/"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if ct != "text/html; charset=utf-8" {
			t.Errorf("GET %s: Content-Type = %q, want %q", path, ct, "text/html; charset=utf-8")
		}
	}
}

// TestMenuDayMealsInSlotOrder asserts every day carries exactly the four
// meals in breakfast/lunch/dinner/snack order.
func TestMenuDayMealsInSlotOrder(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30")
	days, _ := body["days"].([]any)
	if len(days) != 7 {
		t.Fatalf("days length = %d, want 7", len(days))
	}
	wantSlots := []string{"breakfast", "lunch", "dinner", "snack"}
	for _, d := range days {
		dm, _ := d.(map[string]any)
		meals, _ := dm["meals"].([]any)
		if len(meals) != 4 {
			t.Fatalf("day %v: meals length = %d, want 4", dm["day"], len(meals))
		}
		for i, wantSlot := range wantSlots {
			mm, _ := meals[i].(map[string]any)
			if got, _ := mm["slot"].(string); got != wantSlot {
				t.Errorf("day %v: meals[%d].slot = %q, want %q", dm["day"], i, got, wantSlot)
			}
		}
	}
}

// TestMenuLunchWeekdayVsWeekend asserts Mon-Fri get lunch.weekday and
// Sat/Sun get lunch.weekend.
func TestMenuLunchWeekdayVsWeekend(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30")

	mon := findDay(t, body, "mon")
	monLunch := findMealBySlot(t, mon, "lunch")
	if name, _ := monLunch["name"].(string); name != "Chicken bowl" {
		t.Errorf("mon lunch name = %q, want %q", name, "Chicken bowl")
	}

	for _, key := range []string{"sat", "sun"} {
		day := findDay(t, body, key)
		lunch := findMealBySlot(t, day, "lunch")
		if name, _ := lunch["name"].(string); name != "Tuna and avocado plate" {
			t.Errorf("%s lunch name = %q, want %q", key, name, "Tuna and avocado plate")
		}
	}
}

// TestMenuSaturdayDinnerMealFollowsWeekAB asserts the dinner slot inside
// days[].meals resolves the same A/B sub-map as days[].dinner does.
func TestMenuSaturdayDinnerMealFollowsWeekAB(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, bodyA := getMenuJSON(t, mux, "/api/menu?date=2026-08-30") // week A
	satA := findDay(t, bodyA, "sat")
	dinnerMealA := findMealBySlot(t, satA, "dinner")
	if name, _ := dinnerMealA["name"].(string); name != "Beef stir fry" {
		t.Errorf("week A sat dinner meal name = %q, want %q", name, "Beef stir fry")
	}

	_, bodyB := getMenuJSON(t, mux, "/api/menu?date=2026-09-06") // week B
	satB := findDay(t, bodyB, "sat")
	dinnerMealB := findMealBySlot(t, satB, "dinner")
	if name, _ := dinnerMealB["name"].(string); name != "Steak and asparagus (long-ride night)" {
		t.Errorf("week B sat dinner meal name = %q, want %q", name, "Steak and asparagus (long-ride night)")
	}
}

// TestMenuMealRecipeIsStringList asserts a meal with a recipe: list in
// menu.yaml comes through as a []string in the JSON response.
func TestMenuMealRecipeIsStringList(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30")
	sun := findDay(t, body, "sun")
	dinner := findMealBySlot(t, sun, "dinner")
	recipe, ok := dinner["recipe"].([]any)
	if !ok {
		t.Fatalf("sun dinner recipe not a list: %v (%T)", dinner["recipe"], dinner["recipe"])
	}
	want := []string{"Oven 400F.", "Roast the salmon and broccoli."}
	if len(recipe) != len(want) {
		t.Fatalf("sun dinner recipe length = %d, want %d", len(recipe), len(want))
	}
	for i, w := range want {
		if got, _ := recipe[i].(string); got != w {
			t.Errorf("sun dinner recipe[%d] = %q, want %q", i, got, w)
		}
	}

	breakfast := findMealBySlot(t, sun, "breakfast")
	bfRecipe, ok := breakfast["recipe"].([]any)
	if !ok || len(bfRecipe) != 2 {
		t.Errorf("sun breakfast recipe = %v, want a 2-step list", breakfast["recipe"])
	}
}

// TestMenuMealMissingRecipeReturnsEmptyList asserts a meal whose YAML
// entry omits recipe entirely still returns [] (not null/missing) so the
// page's "How to make it" list logic doesn't need a null check.
func TestMenuMealMissingRecipeReturnsEmptyList(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30")
	mon := findDay(t, body, "mon")
	dinner := findMealBySlot(t, mon, "dinner") // mon has no recipe: key in the fixture
	recipe, ok := dinner["recipe"].([]any)
	if !ok {
		t.Fatalf("mon dinner recipe not a list: %v (%T)", dinner["recipe"], dinner["recipe"])
	}
	if len(recipe) != 0 {
		t.Errorf("mon dinner recipe = %v, want empty list", recipe)
	}
}

// findMealBySlot is a test helper that pulls one meal out of a decoded
// day entry's "meals" array by slot name.
func findMealBySlot(t *testing.T, day map[string]any, slot string) map[string]any {
	t.Helper()
	meals, _ := day["meals"].([]any)
	for _, m := range meals {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if mm["slot"] == slot {
			return mm
		}
	}
	t.Fatalf("slot %q not found in day meals: %v", slot, day["meals"])
	return nil
}

// findDay is a test helper that pulls the entry for a given day key out
// of the decoded /api/menu JSON body's "days" array.
func findDay(t *testing.T, body map[string]any, day string) map[string]any {
	t.Helper()
	days, _ := body["days"].([]any)
	for _, d := range days {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if dm["day"] == day {
			return dm
		}
	}
	t.Fatalf("day %q not found in response days: %v", day, body["days"])
	return nil
}

// findGrocerySection is a test helper that pulls a named section (e.g.
// "Proteins") out of the decoded grocery.sections array.
func findGrocerySection(t *testing.T, grocery map[string]any, name string) map[string]any {
	t.Helper()
	sections, _ := grocery["sections"].([]any)
	for _, s := range sections {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if sm["name"] == name {
			return sm
		}
	}
	t.Fatalf("section %q not found in grocery.sections: %v", name, grocery["sections"])
	return nil
}

// findGroceryItem is a test helper that pulls one item by name out of a
// decoded []any of grocery items (a section's "items", or "restock").
func findGroceryItem(items []any, name string) map[string]any {
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if im["item"] == name {
			return im
		}
	}
	return nil
}

// TestMenuGroceryWeekAFiltering asserts Week A's grocery block: week-A-only
// items are included, week-B-only items are excluded, week_a_extra_usd is
// folded into the item's shown est_usd and the week total, and as_needed
// items never appear in a section.
func TestMenuGroceryWeekAFiltering(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30") // week A
	if week, _ := body["week"].(string); week != "A" {
		t.Fatalf("expected week A fixture date, got week=%q", week)
	}
	grocery, _ := body["grocery"].(map[string]any)
	if grocery == nil {
		t.Fatalf("response missing grocery block: %v", body)
	}

	if budget, _ := grocery["budget_usd"].(float64); budget != 100 {
		t.Errorf("budget_usd = %v, want 100", grocery["budget_usd"])
	}

	proteins, _ := findGrocerySection(t, grocery, "Proteins")["items"].([]any)
	sirloin := findGroceryItem(proteins, "Sirloin")
	if sirloin == nil {
		t.Fatalf("Sirloin not found in week A proteins: %v", proteins)
	}
	if est, _ := sirloin["est_usd"].(float64); est != 12.00 {
		t.Errorf("week A sirloin est_usd = %v, want 12.00 (6.00 base + 6.00 week_a_extra_usd)", sirloin["est_usd"])
	}

	produce, _ := findGrocerySection(t, grocery, "Produce")["items"].([]any)
	if item := findGroceryItem(produce, "Asparagus"); item != nil {
		t.Errorf("week A produce should not include the week:B-only Asparagus, got %v", item)
	}
	if item := findGroceryItem(produce, "Spinach"); item == nil {
		t.Errorf("week A produce missing Spinach: %v", produce)
	}

	kids, _ := findGrocerySection(t, grocery, "Kids")["items"].([]any)
	if item := findGroceryItem(kids, "Burger buns"); item == nil {
		t.Errorf("week A kids missing the week:A-only Burger buns: %v", kids)
	}
	if item := findGroceryItem(kids, "Chicken nuggets, frozen"); item != nil {
		t.Errorf("as_needed nuggets should not appear in the kids section, got %v", item)
	}

	// 10.00 (beef) + 12.00 (sirloin+extra) + 4.00 (spinach) + 2.50 (buns) = 28.50
	if total, _ := grocery["week_total_usd"].(float64); total != 28.50 {
		t.Errorf("week A week_total_usd = %v, want 28.50", grocery["week_total_usd"])
	}
}

// TestMenuGroceryWeekBFiltering is TestMenuGroceryWeekAFiltering's mirror
// for Week B: the week:B-only item is included, the week:A-only item is
// excluded, and week_a_extra_usd does not apply.
func TestMenuGroceryWeekBFiltering(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-09-06") // week B
	if week, _ := body["week"].(string); week != "B" {
		t.Fatalf("expected week B fixture date, got week=%q", week)
	}
	grocery, _ := body["grocery"].(map[string]any)
	if grocery == nil {
		t.Fatalf("response missing grocery block: %v", body)
	}

	proteins, _ := findGrocerySection(t, grocery, "Proteins")["items"].([]any)
	sirloin := findGroceryItem(proteins, "Sirloin")
	if sirloin == nil {
		t.Fatalf("Sirloin not found in week B proteins: %v", proteins)
	}
	if est, _ := sirloin["est_usd"].(float64); est != 6.00 {
		t.Errorf("week B sirloin est_usd = %v, want 6.00 (no week_a_extra_usd outside week A)", sirloin["est_usd"])
	}

	produce, _ := findGrocerySection(t, grocery, "Produce")["items"].([]any)
	if item := findGroceryItem(produce, "Asparagus"); item == nil {
		t.Errorf("week B produce missing the week:B-only Asparagus: %v", produce)
	}

	kids, _ := findGrocerySection(t, grocery, "Kids")["items"].([]any)
	if item := findGroceryItem(kids, "Burger buns"); item != nil {
		t.Errorf("week B kids should not include the week:A-only Burger buns, got %v", item)
	}

	// 10.00 (beef) + 6.00 (sirloin) + 3.00 (asparagus) + 4.00 (spinach) = 23.00
	if total, _ := grocery["week_total_usd"].(float64); total != 23.00 {
		t.Errorf("week B week_total_usd = %v, want 23.00", grocery["week_total_usd"])
	}
}

// TestMenuGroceryRestockSeparation asserts an as_needed item lands in
// grocery.restock (not any section) in both weeks, carrying its qty and
// est_usd.
func TestMenuGroceryRestockSeparation(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	for _, date := range []string{"2026-08-30", "2026-09-06"} {
		_, body := getMenuJSON(t, mux, "/api/menu?date="+date)
		grocery, _ := body["grocery"].(map[string]any)
		restock, _ := grocery["restock"].([]any)
		nuggets := findGroceryItem(restock, "Chicken nuggets, frozen")
		if nuggets == nil {
			t.Fatalf("date=%s: restock missing nuggets: %v", date, restock)
		}
		if qty, _ := nuggets["qty"].(string); qty != "1 bag" {
			t.Errorf("date=%s: restock nuggets qty = %q, want %q", date, qty, "1 bag")
		}
		if est, _ := nuggets["est_usd"].(float64); est != 7.00 {
			t.Errorf("date=%s: restock nuggets est_usd = %v, want 7.00", date, est)
		}
		if len(restock) != 1 {
			t.Errorf("date=%s: restock length = %d, want 1", date, len(restock))
		}
	}
}

// TestMenuGroceryTripItemsPassthrough asserts trip_items pass through
// verbatim with their notes.
func TestMenuGroceryTripItemsPassthrough(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30")
	grocery, _ := body["grocery"].(map[string]any)
	tripItems, _ := grocery["trip_items"].([]any)
	if len(tripItems) != 1 {
		t.Fatalf("trip_items length = %d, want 1: %v", len(tripItems), tripItems)
	}
	bananas, _ := tripItems[0].(map[string]any)
	if item, _ := bananas["item"].(string); item != "Bananas" {
		t.Errorf("trip_items[0].item = %q, want %q", item, "Bananas")
	}
	if note, _ := bananas["note"].(string); note != "pre-workout" {
		t.Errorf("trip_items[0].note = %q, want %q", note, "pre-workout")
	}
}

// findPrepToday is a test helper that pulls the decoded prep_today array
// out of a /api/menu response body.
func findPrepToday(t *testing.T, body map[string]any) []any {
	t.Helper()
	prepToday, ok := body["prep_today"].([]any)
	if !ok {
		t.Fatalf("prep_today not a list: %v (%T)", body["prep_today"], body["prep_today"])
	}
	return prepToday
}

// prepTodayEntry is a test helper that finds one prep_today entry by
// "when" bucket, returning nil if none match.
func prepTodayEntry(prepToday []any, when string) map[string]any {
	for _, p := range prepToday {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if pm["when"] == when {
			return pm
		}
	}
	return nil
}

// TestMenuPrepAheadOnMeal asserts a dinner's prep_ahead: list comes through
// on both days[].dinner and the dinner entry of days[].meals, and that a
// dinner without prep_ahead in the YAML still returns [] (not null).
func TestMenuPrepAheadOnMeal(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30") // week A

	thu := findDay(t, body, "thu")
	dinner, _ := thu["dinner"].(map[string]any)
	prepAhead, ok := dinner["prep_ahead"].([]any)
	if !ok || len(prepAhead) != 2 {
		t.Fatalf("thu dinner prep_ahead = %v, want a 2-entry list", dinner["prep_ahead"])
	}

	thuMeal := findMealBySlot(t, thu, "dinner")
	mealPrepAhead, ok := thuMeal["prep_ahead"].([]any)
	if !ok || len(mealPrepAhead) != 2 {
		t.Fatalf("thu dinner meal prep_ahead = %v, want a 2-entry list", thuMeal["prep_ahead"])
	}

	mon := findDay(t, body, "mon")
	monDinner, _ := mon["dinner"].(map[string]any)
	monPrepAhead, ok := monDinner["prep_ahead"].([]any)
	if !ok {
		t.Fatalf("mon dinner prep_ahead not a list: %v (%T)", monDinner["prep_ahead"], monDinner["prep_ahead"])
	}
	if len(monPrepAhead) != 0 {
		t.Errorf("mon dinner prep_ahead = %v, want empty list", monPrepAhead)
	}
}

// TestMenuPrepTodayNightBeforeForTomorrow asserts a Wednesday's prep_today
// includes Thursday's night-before step, labeled for_day "thu" since it is
// done tonight for tomorrow's dinner.
func TestMenuPrepTodayNightBeforeForTomorrow(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-09-02") // Wednesday
	prepToday := findPrepToday(t, body)

	entry := prepTodayEntry(prepToday, "night-before")
	if entry == nil {
		t.Fatalf("prep_today missing a night-before entry: %v", prepToday)
	}
	if forDay, _ := entry["for_day"].(string); forDay != "thu" {
		t.Errorf("night-before entry for_day = %q, want %q", forDay, "thu")
	}
	if forDate, _ := entry["for_date"].(string); forDate != "2026-09-03" {
		t.Errorf("night-before entry for_date = %q, want %q", forDate, "2026-09-03")
	}
	if dinner, _ := entry["dinner"].(string); dinner != "Beef meatballs and marinara" {
		t.Errorf("night-before entry dinner = %q, want %q", dinner, "Beef meatballs and marinara")
	}
	if step, _ := entry["step"].(string); step == "" {
		t.Errorf("night-before entry step is empty")
	}

	// Wednesday is not Sunday and today's own dinner (Baked cod) has no
	// prep_ahead, so no other entries should appear.
	if len(prepToday) != 1 {
		t.Errorf("Wednesday prep_today length = %d, want 1: %v", len(prepToday), prepToday)
	}
}

// TestMenuPrepTodayMorningOfAndBeforeDinner asserts a Friday's prep_today
// includes both today's morning-of and before-dinner steps, in that order.
func TestMenuPrepTodayMorningOfAndBeforeDinner(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-09-04") // Friday
	prepToday := findPrepToday(t, body)

	morningOf := prepTodayEntry(prepToday, "morning-of")
	if morningOf == nil {
		t.Fatalf("prep_today missing a morning-of entry: %v", prepToday)
	}
	if forDay, _ := morningOf["for_day"].(string); forDay != "fri" {
		t.Errorf("morning-of entry for_day = %q, want %q", forDay, "fri")
	}

	beforeDinner := prepTodayEntry(prepToday, "before-dinner")
	if beforeDinner == nil {
		t.Fatalf("prep_today missing a before-dinner entry: %v", prepToday)
	}
	if forDay, _ := beforeDinner["for_day"].(string); forDay != "fri" {
		t.Errorf("before-dinner entry for_day = %q, want %q", forDay, "fri")
	}

	// Order: sunday-cook, morning-of, night-before, before-dinner.
	if len(prepToday) != 2 {
		t.Fatalf("Friday prep_today length = %d, want 2: %v", len(prepToday), prepToday)
	}
	if when, _ := prepToday[0].(map[string]any)["when"].(string); when != "morning-of" {
		t.Errorf("prep_today[0].when = %q, want %q", when, "morning-of")
	}
	if when, _ := prepToday[1].(map[string]any)["when"].(string); when != "before-dinner" {
		t.Errorf("prep_today[1].when = %q, want %q", when, "before-dinner")
	}
}

// TestMenuPrepTodaySundayCook asserts a Sunday's prep_today includes the
// coming week's sunday-cook steps (here, Thursday's).
func TestMenuPrepTodaySundayCook(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30") // Sunday
	prepToday := findPrepToday(t, body)

	entry := prepTodayEntry(prepToday, "sunday-cook")
	if entry == nil {
		t.Fatalf("prep_today missing a sunday-cook entry: %v", prepToday)
	}
	if forDay, _ := entry["for_day"].(string); forDay != "thu" {
		t.Errorf("sunday-cook entry for_day = %q, want %q", forDay, "thu")
	}
	if dinner, _ := entry["dinner"].(string); dinner != "Beef meatballs and marinara" {
		t.Errorf("sunday-cook entry dinner = %q, want %q", dinner, "Beef meatballs and marinara")
	}

	// Sunday's own dinner (salmon) and Monday's (chicken thighs) have no
	// prep_ahead, so the sunday-cook entry should be the only one.
	if len(prepToday) != 1 {
		t.Errorf("Sunday prep_today length = %d, want 1: %v", len(prepToday), prepToday)
	}
}

// TestMenuPrepTodayEmpty asserts a day with nothing to prep yields an
// empty (non-nil) prep_today array.
func TestMenuPrepTodayEmpty(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-31") // Monday
	prepToday := findPrepToday(t, body)
	if len(prepToday) != 0 {
		t.Errorf("Monday prep_today = %v, want empty list", prepToday)
	}
}

// TestMenuGroceryMonthlyPantry asserts monthly_pantry's total and the
// mb_or_wf flag pass through.
func TestMenuGroceryMonthlyPantry(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30")
	grocery, _ := body["grocery"].(map[string]any)
	pantry, _ := grocery["monthly_pantry"].(map[string]any)
	if pantry == nil {
		t.Fatalf("grocery missing monthly_pantry: %v", grocery)
	}
	if total, _ := pantry["total_usd"].(float64); total != 16.00 {
		t.Errorf("monthly_pantry.total_usd = %v, want 16.00", pantry["total_usd"])
	}
	items, _ := pantry["items"].([]any)
	aminos := findGroceryItem(items, "Coconut aminos")
	if aminos == nil {
		t.Fatalf("monthly_pantry missing Coconut aminos: %v", items)
	}
	if mbOrWf, _ := aminos["mb_or_wf"].(bool); !mbOrWf {
		t.Errorf("Coconut aminos mb_or_wf = %v, want true", aminos["mb_or_wf"])
	}
	oil := findGroceryItem(items, "Olive oil")
	if oil == nil {
		t.Fatalf("monthly_pantry missing Olive oil: %v", items)
	}
	if _, ok := oil["mb_or_wf"]; ok {
		t.Errorf("Olive oil should omit mb_or_wf (false, omitempty), got %v", oil["mb_or_wf"])
	}
}

// TestMenuKidRulesKeyedByName asserts kid_rules is a map keyed by each
// kid's menu.yaml name: only kids with a non-empty rules list appear, and a
// kid without rules (Jack, in the fixture) is absent entirely.
func TestMenuKidRulesKeyedByName(t *testing.T) {
	mux := newMenuTestMux(t, menuFixtureYAML)

	_, body := getMenuJSON(t, mux, "/api/menu?date=2026-08-30")
	kidRules, _ := body["kid_rules"].(map[string]any)
	if len(kidRules) != 2 {
		t.Fatalf("kid_rules = %v, want exactly 2 entries", kidRules)
	}

	mia, _ := kidRules["Mia"].([]any)
	wantMia := []string{
		"Does not eat chicken or eggs Ryan cooks.",
		"Nuggets stay in the freezer as her fallback.",
	}
	if len(mia) != len(wantMia) {
		t.Fatalf("kid_rules[Mia] = %v, want %v", mia, wantMia)
	}
	for i, rule := range wantMia {
		if got, _ := mia[i].(string); got != rule {
			t.Errorf("kid_rules[Mia][%d] = %q, want %q", i, got, rule)
		}
	}

	sam, _ := kidRules["Sam"].([]any)
	if len(sam) != 1 {
		t.Fatalf("kid_rules[Sam] = %v, want 1 entry", sam)
	}
	if got, _ := sam[0].(string); got != "No tree nuts, ever." {
		t.Errorf("kid_rules[Sam][0] = %q, want %q", got, "No tree nuts, ever.")
	}

	if _, ok := kidRules["Jack"]; ok {
		t.Errorf("kid_rules should not contain Jack (no rules in fixture), got %v", kidRules["Jack"])
	}
}
