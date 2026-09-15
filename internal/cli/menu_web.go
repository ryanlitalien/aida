package cli

import (
	_ "embed"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed menu_web.html
var menuWebHTML []byte

// dayKeys is the Sun-Sat order used throughout: it matches both the
// dinners.yaml map keys and time.Weekday's Sunday==0 enumeration, so a
// weekday index can be used directly against it.
var dayKeys = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// ---------------------------------------------------------------------
// Defensive extraction helpers over generically-decoded YAML
// (map[string]any / []any, yaml.v3's default shape for `any` targets).
// Every helper degrades to a zero value on nil/wrong-type input --
// nothing here may panic.
// ---------------------------------------------------------------------

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		// YAML integers can arrive as float64 through generic decoding;
		// render 8 as "8", 6.5 as "6.5".
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// asStringSlice converts a []any of scalars into a []string, skipping
// any element that isn't a string. Returns nil (never panics) for a
// missing/wrong-typed input.
func asStringSlice(v any) []string {
	items := asSlice(v)
	if items == nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s := asString(it)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// asTime handles the yaml.v3 auto-resolved time.Time case for an
// unquoted ISO date scalar, falling back to parsing a plain
// "2006-01-02" string in case the YAML author quotes the value later.
// Returns the zero time and false on anything else.
func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case string:
		if parsed, err := time.Parse("2006-01-02", t); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------
// Response shape
// ---------------------------------------------------------------------

type menuDinner struct {
	Name       string         `json:"name"`
	Kids       bool           `json:"kids"`
	KidVersion string         `json:"kid_version,omitempty"`
	Note       string         `json:"note,omitempty"`
	PrepAhead  []menuPrepStep `json:"prep_ahead"`
}

// menuPrepStep is one entry of a dinner's prep_ahead: list -- a single
// prep task tied to a "when" bucket (sunday-cook, night-before,
// morning-of, before-dinner).
type menuPrepStep struct {
	When string `json:"when"`
	Step string `json:"step"`
}

// menuPrepToday is one entry of the top-level prep_today list: a prep step
// that matters TODAY, tagged with which day/dinner it is for so the page
// can label it ("for tonight", "for tomorrow: <dinner>", "Sunday cook").
type menuPrepToday struct {
	ForDay  string `json:"for_day"`
	ForDate string `json:"for_date"`
	Dinner  string `json:"dinner"`
	When    string `json:"when"`
	Step    string `json:"step"`
}

// menuItem is one line of a meal's items[] list (food/qty/macros) as read
// from menu.yaml, or the training_modifier's single "pre" item.
type menuItem struct {
	Food string `json:"food"`
	Qty  string `json:"qty"`
	Cal  int    `json:"cal"`
	P    int    `json:"p"`
	C    int    `json:"c"`
	F    int    `json:"f"`
}

// menuTotals is a meal's macro totals block.
type menuTotals struct {
	Cal int `json:"cal"`
	P   int `json:"p"`
	C   int `json:"c"`
	F   int `json:"f"`
}

// menuMeal is the full per-meal record: everything the right-column
// recipe/detail view needs, for any of the four slots. Recipe and Items
// are always non-nil (empty list, not null) even when menu.yaml omits
// them entirely -- see extractMeal.
type menuMeal struct {
	Slot       string         `json:"slot"`
	Name       string         `json:"name"`
	Note       string         `json:"note,omitempty"`
	KidVersion string         `json:"kid_version,omitempty"`
	Kids       bool           `json:"kids"`
	Items      []menuItem     `json:"items"`
	Totals     menuTotals     `json:"totals"`
	Recipe     []string       `json:"recipe"`
	Prep       string         `json:"prep,omitempty"`
	PrepAhead  []menuPrepStep `json:"prep_ahead"`
}

type menuDayEntry struct {
	Day    string     `json:"day"`
	Date   string     `json:"date"`
	Dinner menuDinner `json:"dinner"`
	// Meals is the day's full meal set in serving order: breakfast, lunch,
	// dinner, snack. Breakfast/snack are the same every day; lunch is
	// lunch.weekday (Mon-Fri) or lunch.weekend (Sat/Sun); dinner is this
	// day's dinner (Saturday resolved to the week's A/B sub-map).
	Meals   []menuMeal `json:"meals"`
	IsToday bool       `json:"is_today"`
}

type menuNamed struct {
	Name string `json:"name"`
}

type menuLunch struct {
	Weekday menuNamed `json:"weekday"`
	Weekend menuNamed `json:"weekend"`
}

type menuBreakfast struct {
	Name         string `json:"name"`
	ItemsSummary string `json:"items_summary"`
}

type menuSundayCook struct {
	Steps        []string `json:"steps"`
	ThursdayRule string   `json:"thursday_rule"`
}

// menuTrainingModifier mirrors the top-level training_modifier block: the
// carb-unit rule applied on training days, plus the pre-workout item and
// the carb-unit option list.
type menuTrainingModifier struct {
	Name            string     `json:"name"`
	Rule            string     `json:"rule"`
	Pre             menuItem   `json:"pre"`
	CarbUnitOptions []menuItem `json:"carb_unit_options"`
}

// menuGroceryItem is one line of a grocery section or the restock list:
// {item, qty, for, est_usd}. For is omitted (empty) on restock entries --
// the API contract for restock is the narrower {item, qty, est_usd}.
type menuGroceryItem struct {
	Item   string  `json:"item"`
	Qty    string  `json:"qty"`
	For    string  `json:"for,omitempty"`
	EstUSD float64 `json:"est_usd"`
}

// menuGrocerySection is one of the three weekly sections (Proteins,
// Produce, Kids) after week-A/B filtering and as_needed exclusion.
type menuGrocerySection struct {
	Name  string            `json:"name"`
	Items []menuGroceryItem `json:"items"`
}

// menuGroceryTripItem is one of grocery.trip_items -- variable items Ryan
// buys on separate trips rather than in the standing order.
type menuGroceryTripItem struct {
	Item string `json:"item"`
	Note string `json:"note"`
}

// menuGroceryPantryItem is one line of grocery.monthly_pantry.
type menuGroceryPantryItem struct {
	Item   string  `json:"item"`
	Qty    string  `json:"qty"`
	EstUSD float64 `json:"est_usd"`
	MbOrWf bool    `json:"mb_or_wf,omitempty"`
}

// menuGroceryPantry wraps the monthly pantry list with its running total.
type menuGroceryPantry struct {
	TotalUSD float64                 `json:"total_usd"`
	Items    []menuGroceryPantryItem `json:"items"`
}

// menuGrocery is the /api/menu "grocery" block: the current week's
// (A or B, per the same week the rest of the page renders) shopping list,
// derived from menu.yaml's grocery: block. See extractGrocery for the
// filtering rules (week inclusion, as_needed -> restock, week_a_extra_usd).
type menuGrocery struct {
	BudgetUSD     float64               `json:"budget_usd"`
	WeekTotalUSD  float64               `json:"week_total_usd"`
	Sections      []menuGrocerySection  `json:"sections"`
	Restock       []menuGroceryItem     `json:"restock"`
	TripItems     []menuGroceryTripItem `json:"trip_items"`
	MonthlyPantry menuGroceryPantry     `json:"monthly_pantry"`
}

type menuResponse struct {
	Week      string `json:"week"`
	WeekStart string `json:"week_start"`
	WeekEnd   string `json:"week_end"`
	Today     string `json:"today"`
	// LAN is true when this response was served off the LAN-bound listener
	// (registerMenuWebRoutes's isLAN param) rather than the main loopback
	// daemon. The page uses this to hide its <nav>: the nav's links
	// (Jarvis/Tasks/Runs/Dashboard/Bifrost) only exist on the loopback
	// daemon, so on the LAN listener every one of them 404s.
	LAN  bool     `json:"lan"`
	Kids []string `json:"kids"`
	// KidRules maps a kid's name (as it appears in menu.yaml's
	// people.kids list) to their dietary rules, for any kid that has a
	// non-empty rules list. Keyed by name rather than a single fixed
	// field so the binary never hardcodes a real child's name.
	KidRules map[string][]string `json:"kid_rules,omitempty"`
	// PrepToday is the "prep radar" list: prep_ahead steps that matter
	// TODAY (refDate), computed by buildMenuResponse -- see its doc
	// comment for the exact inclusion/ordering rules.
	PrepToday        []menuPrepToday      `json:"prep_today"`
	Days             []menuDayEntry       `json:"days"`
	Breakfast        menuBreakfast        `json:"breakfast"`
	Lunch            menuLunch            `json:"lunch"`
	Snack            menuNamed            `json:"snack"`
	KidsWeekendFood  []string             `json:"kids_weekend_food"`
	SundayCook       menuSundayCook       `json:"sunday_cook"`
	TrainingModifier menuTrainingModifier `json:"training_modifier"`
	Grocery          menuGrocery          `json:"grocery"`
}

// ---------------------------------------------------------------------
// Week Sun-Sat / A-B math
// ---------------------------------------------------------------------

// weekStart returns the Sunday that starts the Sun-Sat week containing d,
// normalized to noon to sidestep any DST-transition day-length weirdness
// in later date arithmetic.
func weekStart(d time.Time) time.Time {
	y, m, day := d.Date()
	noon := time.Date(y, m, day, 12, 0, 0, 0, d.Location())
	return noon.AddDate(0, 0, -int(noon.Weekday())) // time.Sunday == 0
}

// weekLabel computes the "A"/"B" label for the week starting at ws,
// relative to the given anchor date (only the anchor's calendar date is
// used; its time-of-day/location are irrelevant).
func weekLabel(ws time.Time, anchor time.Time) string {
	friday := ws.AddDate(0, 0, 5)
	ay, am, ad := anchor.Date()
	anchorNoon := time.Date(ay, am, ad, 12, 0, 0, 0, friday.Location())
	diffDays := int(friday.Sub(anchorNoon).Hours() / 24)
	if ((diffDays%14)+14)%14 == 0 {
		return "A"
	}
	return "B"
}

// ---------------------------------------------------------------------
// YAML -> response
// ---------------------------------------------------------------------

func loadMenuYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	return top, nil
}

// asInt handles every numeric shape yaml.v3 produces for an `any` target
// (int for small integers, int64/uint64 for large ones, float64 for
// anything written with a decimal point). Returns 0 on anything else.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// asFloat is asInt's float64 counterpart, for the grocery block's
// est_usd/budget_usd fields (a mix of plain integers like `budget_usd: 150`
// and decimals like `est_usd: 9.00` -- both need to land as float64).
// Returns 0 on anything else.
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	}
	return 0
}

// roundCents rounds a dollar amount to the nearest cent, guarding against
// float64 accumulation drift when summing many est_usd values.
func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}

// nonNilStrings returns v (already run through asStringSlice) unless it is
// nil, in which case it returns an empty (non-nil) slice -- so the JSON
// encoder emits [] rather than null for an absent list.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// extractItems reads a meal's items[] list into typed menuItems, skipping
// any element that isn't a map. Never returns nil -- an absent or
// malformed items key yields an empty (non-nil) slice.
func extractItems(raw any) []menuItem {
	items := asSlice(raw)
	out := make([]menuItem, 0, len(items))
	for _, it := range items {
		m := asMap(it)
		if m == nil {
			continue
		}
		out = append(out, extractItem(m))
	}
	return out
}

// extractItem reads a single {food, qty, cal, p, c, f} map into a menuItem.
func extractItem(raw any) menuItem {
	m := asMap(raw)
	return menuItem{
		Food: asString(m["food"]),
		Qty:  asString(m["qty"]),
		Cal:  asInt(m["cal"]),
		P:    asInt(m["p"]),
		C:    asInt(m["c"]),
		F:    asInt(m["f"]),
	}
}

// extractTotals reads a meal's totals: {cal, p, c, f} block.
func extractTotals(raw any) menuTotals {
	m := asMap(raw)
	return menuTotals{
		Cal: asInt(m["cal"]),
		P:   asInt(m["p"]),
		C:   asInt(m["c"]),
		F:   asInt(m["f"]),
	}
}

// extractPrepAhead reads a meal's prep_ahead: [{when, step}] list into
// typed menuPrepSteps, skipping any element that isn't a map. Never
// returns nil -- an absent or malformed prep_ahead key yields an empty
// (non-nil) slice, matching extractItems/extractItem's shape.
func extractPrepAhead(raw any) []menuPrepStep {
	items := asSlice(raw)
	out := make([]menuPrepStep, 0, len(items))
	for _, it := range items {
		m := asMap(it)
		if m == nil {
			continue
		}
		out = append(out, menuPrepStep{
			When: asString(m["when"]),
			Step: asString(m["step"]),
		})
	}
	return out
}

// extractMeal reads one meal entry (breakfast, a lunch variant, a dinner
// entry, or snack -- all the same map shape) into a full menuMeal for the
// given slot. Applies the same kids_weeks filter extractDinner used to
// apply on its own: when kids_weeks is present and non-empty, the
// effective kids bool is raw_kids AND label is in kids_weeks; when
// kids_weeks is absent, the raw kids bool is authoritative. Never panics
// on a nil/malformed raw -- degrades to a mostly-zero menuMeal with
// Items/Recipe as empty (non-nil) lists.
func extractMeal(raw any, slot string, label string) menuMeal {
	m := asMap(raw)
	if m == nil {
		return menuMeal{Slot: slot, Items: []menuItem{}, Recipe: []string{}, PrepAhead: []menuPrepStep{}}
	}
	rawKids := asBool(m["kids"])
	effectiveKids := rawKids
	kidsWeeks := asStringSlice(m["kids_weeks"])
	if len(kidsWeeks) > 0 {
		inWeek := false
		for _, w := range kidsWeeks {
			if w == label {
				inWeek = true
				break
			}
		}
		effectiveKids = rawKids && inWeek
	}
	return menuMeal{
		Slot:       slot,
		Name:       asString(m["name"]),
		Note:       asString(m["note"]),
		KidVersion: asString(m["kid_version"]),
		Kids:       effectiveKids,
		Items:      extractItems(m["items"]),
		Totals:     extractTotals(m["totals"]),
		Recipe:     nonNilStrings(asStringSlice(m["recipe"])),
		Prep:       asString(m["prep"]),
		PrepAhead:  extractPrepAhead(m["prep_ahead"]),
	}
}

// extractDinner is extractMeal specialized to the "dinner" slot, returning
// just the legacy menuDinner fields (name/kids/kid_version/note/prep_ahead)
// that days[].dinner has always carried.
func extractDinner(raw any, label string) menuDinner {
	meal := extractMeal(raw, "dinner", label)
	return menuDinner{
		Name:       meal.Name,
		Kids:       meal.Kids,
		KidVersion: meal.KidVersion,
		Note:       meal.Note,
		PrepAhead:  meal.PrepAhead,
	}
}

// dinnerRawForDate returns the raw dinner YAML node for an arbitrary date,
// not necessarily within the response's own week -- used to look up
// tomorrow's dinner for night-before prep steps, which can fall in the
// following Sun-Sat week (e.g. today is Saturday, tomorrow is next
// Sunday). Resolves Saturday's A/B sub-map against the week label
// computed for date's own Sun-Sat week, exactly like buildMenuResponse's
// per-day loop does for the response's own week.
func dinnerRawForDate(dinnersMap map[string]any, anchorTime, date time.Time) any {
	key := dayKeys[int(date.Weekday())]
	if key != "sat" {
		return dinnersMap[key]
	}
	label := weekLabel(weekStart(date), anchorTime)
	satMap := asMap(dinnersMap[key])
	return satMap[label]
}

// groceryWeeklySections is the fixed key->display-name mapping for
// grocery.weekly's three sub-lists, in display order.
var groceryWeeklySections = []struct{ key, name string }{
	{"proteins", "Proteins"},
	{"produce", "Produce"},
	{"kids", "Kids"},
}

// extractGrocery reads the top-level grocery: block into the current
// week's shopping list, for the same A/B label the rest of the page uses.
//
// Filtering rules per weekly item:
//   - as_needed: true items never appear in a section; they land in
//     restock instead (stock-when-out items like the kids' backup nuggets).
//   - a week: A|B item is dropped entirely when it doesn't match label.
//   - when label is "A", week_a_extra_usd (if present) is added onto the
//     item's shown est_usd -- this applies independent of the item's own
//     week field (e.g. the sirloin line has no week restriction but costs
//     more in Week A, when the kids also get stir fry off it).
//
// week_total_usd is the sum of the three sections' shown est_usd only --
// restock/trip_items/monthly_pantry are informational and excluded from it.
// Never panics on missing/malformed input; every slice returned is non-nil.
func extractGrocery(top map[string]any, label string) menuGrocery {
	groceryMap := asMap(top["grocery"])
	weeklyMap := asMap(groceryMap["weekly"])

	sections := make([]menuGrocerySection, 0, len(groceryWeeklySections))
	restock := []menuGroceryItem{}
	weekTotal := 0.0

	for _, sd := range groceryWeeklySections {
		items := asSlice(weeklyMap[sd.key])
		sectionItems := make([]menuGroceryItem, 0, len(items))
		for _, raw := range items {
			m := asMap(raw)
			if m == nil {
				continue
			}
			if asBool(m["as_needed"]) {
				restock = append(restock, menuGroceryItem{
					Item:   asString(m["item"]),
					Qty:    asString(m["qty"]),
					EstUSD: roundCents(asFloat(m["est_usd"])),
				})
				continue
			}
			itemWeek := asString(m["week"])
			if itemWeek != "" && itemWeek != label {
				continue
			}
			est := asFloat(m["est_usd"])
			if label == "A" {
				est += asFloat(m["week_a_extra_usd"])
			}
			est = roundCents(est)
			sectionItems = append(sectionItems, menuGroceryItem{
				Item:   asString(m["item"]),
				Qty:    asString(m["qty"]),
				For:    asString(m["for"]),
				EstUSD: est,
			})
			weekTotal += est
		}
		sections = append(sections, menuGrocerySection{Name: sd.name, Items: sectionItems})
	}

	tripRaw := asSlice(groceryMap["trip_items"])
	tripItems := make([]menuGroceryTripItem, 0, len(tripRaw))
	for _, raw := range tripRaw {
		m := asMap(raw)
		if m == nil {
			continue
		}
		tripItems = append(tripItems, menuGroceryTripItem{
			Item: asString(m["item"]),
			Note: asString(m["note"]),
		})
	}

	pantryRaw := asSlice(groceryMap["monthly_pantry"])
	pantryItems := make([]menuGroceryPantryItem, 0, len(pantryRaw))
	pantryTotal := 0.0
	for _, raw := range pantryRaw {
		m := asMap(raw)
		if m == nil {
			continue
		}
		est := roundCents(asFloat(m["est_usd"]))
		pantryTotal += est
		pantryItems = append(pantryItems, menuGroceryPantryItem{
			Item:   asString(m["item"]),
			Qty:    asString(m["qty"]),
			EstUSD: est,
			MbOrWf: asBool(m["mb_or_wf"]),
		})
	}

	return menuGrocery{
		BudgetUSD:    roundCents(asFloat(groceryMap["budget_usd"])),
		WeekTotalUSD: roundCents(weekTotal),
		Sections:     sections,
		Restock:      restock,
		TripItems:    tripItems,
		MonthlyPantry: menuGroceryPantry{
			TotalUSD: roundCents(pantryTotal),
			Items:    pantryItems,
		},
	}
}

// buildMenuResponse turns the generically-decoded YAML document into the
// typed API response for the week containing refDate.
//
// It also computes PrepToday, the "prep radar" list: the prep_ahead steps
// that matter TODAY (refDate), so Ryan never discovers at 6pm that
// something should have moved from freezer to fridge last night. In
// order:
//  1. sunday-cook -- every sunday-cook step across the whole coming week's
//     dinners, but only when refDate itself is a Sunday (the day the cook
//     happens).
//  2. morning-of -- today's own dinner's morning-of steps.
//  3. night-before -- tomorrow's dinner's night-before steps (they get
//     done today, the night before tomorrow's dinner).
//  4. before-dinner -- today's own dinner's before-dinner steps.
func buildMenuResponse(top map[string]any, refDate time.Time) menuResponse {
	ws := weekStart(refDate)

	anchorMap := asMap(top["anchor"])
	anchorTime, _ := asTime(anchorMap["kid_weekend_friday"])
	label := weekLabel(ws, anchorTime)

	dinnersMap := asMap(top["dinners"])
	breakfastMap := asMap(top["breakfast"])
	lunchMap := asMap(top["lunch"])
	lunchWeekdayMap := asMap(lunchMap["weekday"])
	lunchWeekendMap := asMap(lunchMap["weekend"])
	snackMap := asMap(top["snack"])

	days := make([]menuDayEntry, 0, 7)
	refDateStr := refDate.Format("2006-01-02")
	todayKey := ""
	if wd := int(refDate.Weekday()); wd >= 0 && wd < len(dayKeys) {
		todayKey = dayKeys[wd]
	}

	for i, key := range dayKeys {
		date := ws.AddDate(0, 0, i)
		dateStr := date.Format("2006-01-02")

		var dinnerRaw any
		if key == "sat" {
			satMap := asMap(dinnersMap[key])
			dinnerRaw = satMap[label]
		} else {
			dinnerRaw = dinnersMap[key]
		}
		dinner := extractDinner(dinnerRaw, label)

		// lunch.weekday for Mon-Fri, lunch.weekend for Sat/Sun.
		lunchRaw := lunchWeekdayMap
		if key == "sat" || key == "sun" {
			lunchRaw = lunchWeekendMap
		}

		meals := []menuMeal{
			extractMeal(breakfastMap, "breakfast", label),
			extractMeal(lunchRaw, "lunch", label),
			extractMeal(dinnerRaw, "dinner", label),
			extractMeal(snackMap, "snack", label),
		}

		days = append(days, menuDayEntry{
			Day:     key,
			Date:    dateStr,
			Dinner:  dinner,
			Meals:   meals,
			IsToday: dateStr == refDateStr,
		})
	}

	// breakfast (top-level summary field, kept for existing consumers)
	var itemFoods []string
	for _, it := range asSlice(breakfastMap["items"]) {
		itemMap := asMap(it)
		if food := asString(itemMap["food"]); food != "" {
			itemFoods = append(itemFoods, food)
		}
	}
	breakfast := menuBreakfast{
		Name:         asString(breakfastMap["name"]),
		ItemsSummary: strings.Join(itemFoods, ", "),
	}

	// lunch (top-level summary field, kept for existing consumers)
	lunch := menuLunch{
		Weekday: menuNamed{Name: asString(lunchWeekdayMap["name"])},
		Weekend: menuNamed{Name: asString(lunchWeekendMap["name"])},
	}

	// snack (top-level summary field, kept for existing consumers)
	snack := menuNamed{Name: asString(snackMap["name"])}

	// training_modifier
	tmMap := asMap(top["training_modifier"])
	trainingModifier := menuTrainingModifier{
		Name:            asString(tmMap["name"]),
		Rule:            asString(tmMap["rule"]),
		Pre:             extractItem(tmMap["pre"]),
		CarbUnitOptions: extractItems(tmMap["carb_unit_options"]),
	}

	// people.kids
	peopleMap := asMap(top["people"])
	var kidNames []string
	var kidRules map[string][]string
	for _, kid := range asSlice(peopleMap["kids"]) {
		kidMap := asMap(kid)
		name := asString(kidMap["name"])
		if name != "" {
			kidNames = append(kidNames, name)
		}
		if rules := asStringSlice(kidMap["rules"]); name != "" && len(rules) > 0 {
			if kidRules == nil {
				kidRules = make(map[string][]string)
			}
			kidRules[name] = rules
		}
	}

	// kids_weekend_food
	kidsWeekendFood := asStringSlice(top["kids_weekend_food"])

	// sunday_cook
	scMap := asMap(top["sunday_cook"])
	sundayCook := menuSundayCook{
		Steps:        asStringSlice(scMap["steps"]),
		ThursdayRule: asString(scMap["thursday_rule"]),
	}

	// prep_today -- see buildMenuResponse's doc comment for the exact
	// inclusion/ordering rules. Never nil: appended-to only, starting
	// empty, so a day with nothing yields [] rather than null.
	prepToday := []menuPrepToday{}

	var todayDay *menuDayEntry
	for i := range days {
		if days[i].IsToday {
			todayDay = &days[i]
			break
		}
	}

	// (1) sunday-cook: the whole coming week's dinners, only on Sunday.
	if todayKey == "sun" {
		for _, d := range days {
			for _, step := range d.Dinner.PrepAhead {
				if step.When == "sunday-cook" {
					prepToday = append(prepToday, menuPrepToday{
						ForDay: d.Day, ForDate: d.Date, Dinner: d.Dinner.Name,
						When: step.When, Step: step.Step,
					})
				}
			}
		}
	}

	// (2) morning-of: today's own dinner.
	if todayDay != nil {
		for _, step := range todayDay.Dinner.PrepAhead {
			if step.When == "morning-of" {
				prepToday = append(prepToday, menuPrepToday{
					ForDay: todayDay.Day, ForDate: todayDay.Date, Dinner: todayDay.Dinner.Name,
					When: step.When, Step: step.Step,
				})
			}
		}
	}

	// (3) night-before: tomorrow's dinner, resolved independently of the
	// response's own week since tomorrow can fall in the next one.
	tomorrow := refDate.AddDate(0, 0, 1)
	tomorrowRaw := dinnerRawForDate(dinnersMap, anchorTime, tomorrow)
	tomorrowLabel := weekLabel(weekStart(tomorrow), anchorTime)
	tomorrowMeal := extractMeal(tomorrowRaw, "dinner", tomorrowLabel)
	tomorrowKey := dayKeys[int(tomorrow.Weekday())]
	tomorrowDateStr := tomorrow.Format("2006-01-02")
	for _, step := range tomorrowMeal.PrepAhead {
		if step.When == "night-before" {
			prepToday = append(prepToday, menuPrepToday{
				ForDay: tomorrowKey, ForDate: tomorrowDateStr, Dinner: tomorrowMeal.Name,
				When: step.When, Step: step.Step,
			})
		}
	}

	// (4) before-dinner: today's own dinner.
	if todayDay != nil {
		for _, step := range todayDay.Dinner.PrepAhead {
			if step.When == "before-dinner" {
				prepToday = append(prepToday, menuPrepToday{
					ForDay: todayDay.Day, ForDate: todayDay.Date, Dinner: todayDay.Dinner.Name,
					When: step.When, Step: step.Step,
				})
			}
		}
	}

	return menuResponse{
		Week:             label,
		WeekStart:        ws.Format("2006-01-02"),
		WeekEnd:          ws.AddDate(0, 0, 6).Format("2006-01-02"),
		Today:            todayKey,
		Kids:             kidNames,
		KidRules:         kidRules,
		PrepToday:        prepToday,
		Days:             days,
		Breakfast:        breakfast,
		Lunch:            lunch,
		Snack:            snack,
		KidsWeekendFood:  kidsWeekendFood,
		SundayCook:       sundayCook,
		TrainingModifier: trainingModifier,
		Grocery:          extractGrocery(top, label),
	}
}

// ---------------------------------------------------------------------
// HTTP wiring
// ---------------------------------------------------------------------

// registerMenuWebRoutes registers the three read-only dinner-menu routes
// on mux: GET /menu and GET /menu/ (the embedded HTML page) and
// GET /api/menu (the JSON the page fetches). menuPath is the YAML file
// path to read fresh on every /api/menu request -- deliberately
// uncached, so an edit to the file shows up immediately. Method-prefixed
// patterns (Go 1.22+ mux convention used across this package) make the
// stdlib mux auto-405 any other method on these paths.
//
// isLAN stamps every /api/menu response with "lan": true when this mux is
// the LAN-bound listener (see startMenuLANServer in serve.go) rather than
// the main loopback daemon's mux. The page uses that flag to hide its
// nav bar -- the nav's other links (Jarvis/Tasks/Runs/Dashboard/Bifrost)
// only exist on the loopback daemon and would 404 on the LAN listener.
func registerMenuWebRoutes(mux *http.ServeMux, menuPath string, isLAN bool) {
	htmlHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(menuWebHTML)
	}
	mux.HandleFunc("GET /menu", htmlHandler)
	mux.HandleFunc("GET /menu/", htmlHandler)

	mux.HandleFunc("GET /api/menu", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		refDate := time.Now()
		if dateParam := r.URL.Query().Get("date"); dateParam != "" {
			parsed, err := time.ParseInLocation("2006-01-02", dateParam, time.Local)
			if err != nil {
				httpError(w, http.StatusBadRequest, "invalid date: "+err.Error())
				return
			}
			refDate = parsed
		}

		top, err := loadMenuYAML(menuPath)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "loading menu: "+err.Error())
			return
		}

		resp := buildMenuResponse(top, refDate)
		resp.LAN = isLAN
		writeJSON(w, http.StatusOK, resp)
	})
}
