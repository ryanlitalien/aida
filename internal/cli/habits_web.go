package cli

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed habits_web.html
var habitsWebHTML []byte

// habitsWindowDays is the MINIMUM size of the rolling calendar: at least the
// last 14 days ending today, inclusive. The actual window can run a few days
// longer -- see buildHabitsResponse, which snaps the start back to the
// Sunday of the week containing (today - 13 days) so the oldest (bottom)
// calendar row is always a full Sun-Sat week instead of starting mid-week.
const habitsWindowDays = 14

// habitDef is one checkin-backed habit this page tracks in habits.db, in
// display order. "eating-correctly" is a third indicator the page shows,
// but it is computed from food_diary.json + menu.yaml rather than a
// checkins row, so it never appears here and is never toggleable.
type habitDef struct{ slug, name string }

var trackedHabits = []habitDef{
	{"no-gluten", "No gluten"},
	{"workout", "Workout"},
}

func isToggleableHabitSlug(slug string) bool {
	for _, h := range trackedHabits {
		if h.slug == slug {
			return true
		}
	}
	return false
}

func defaultHabitName(slug string) string {
	for _, h := range trackedHabits {
		if h.slug == slug {
			return h.name
		}
	}
	return slug
}

// ---------------------------------------------------------------------
// Response shape
// ---------------------------------------------------------------------

// habitDay is one calendar cell's worth of state. Gluten/Workout are
// "done" | "miss" | "none" ("none" means no checkins row for that habit on
// that date -- distinct from an explicit "miss"). EatingPct is nil when
// there is no data to compute it from (see eatingCorrectlyPct's doc
// comment for the exact rule). WorkoutAuto is true only when Workout is
// "done" AND that "done" came from an auto-detected workouts.db session
// rather than a manual habits.db checkin -- see resolveWorkoutState.
type habitDay struct {
	Date        string `json:"date"`
	DowIndex    int    `json:"dow_index"` // 0=Sun..6=Sat -- lets the page compute its leading calendar padding without re-deriving weekday in JS
	IsToday     bool   `json:"is_today"`
	Gluten      string `json:"gluten"`
	Workout     string `json:"workout"`
	WorkoutAuto bool   `json:"workout_auto,omitempty"`
	EatingKcal  int    `json:"eating_kcal"`
	EatingPct   *int   `json:"eating_pct"`
}

type habitsResponse struct {
	Today      string     `json:"today"`
	RangeStart string     `json:"range_start"`
	RangeEnd   string     `json:"range_end"`
	TargetKcal int        `json:"target_kcal"`
	Days       []habitDay `json:"days"`
}

// ---------------------------------------------------------------------
// habits.db access
// ---------------------------------------------------------------------

// openHabitsDB opens (or creates) the habit-tracker SQLite database at
// path, matching the exact schema `~/dev/health/scripts/habit.py` uses
// (habits + checkins tables) so this page and that CLI stay interchangeable
// -- either can check a habit off and the other sees it. CREATE TABLE IF
// NOT EXISTS is a no-op against the real, already-populated habits.db; it
// only matters for a machine where the personal-health repo hasn't been
// cloned yet, so the page self-heals instead of 500ing forever.
func openHabitsDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	dsn := path + "?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := ensureHabitsSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure schema: %w", err)
	}
	return db, nil
}

// ensureHabitsSchema mirrors habit.py's DDL verbatim (see its module
// docstring / the schema dumped via `sqlite3 habits.db .schema`). The
// `streaks` view is habit.py's own convenience for CLI status output --
// this page doesn't need it, so it's not recreated here.
func ensureHabitsSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS habits (
			id          INTEGER PRIMARY KEY,
			slug        TEXT UNIQUE NOT NULL,
			name        TEXT NOT NULL,
			description TEXT,
			started_on  TEXT NOT NULL,
			active      INTEGER NOT NULL DEFAULT 1,
			created_at  TEXT NOT NULL DEFAULT (datetime('now', 'localtime'))
		)`); err != nil {
		return err
	}
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS checkins (
			id        INTEGER PRIMARY KEY,
			habit_id  INTEGER NOT NULL REFERENCES habits(id) ON DELETE CASCADE,
			date      TEXT NOT NULL,
			status    TEXT NOT NULL DEFAULT 'done' CHECK (status IN ('done', 'miss')),
			note      TEXT,
			logged_at TEXT NOT NULL DEFAULT (datetime('now', 'localtime')),
			UNIQUE (habit_id, date)
		)`)
	return err
}

// ensureHabit creates a habits row for slug if one doesn't already exist --
// the "workout" habit has no row in habits.db yet as of this page's build,
// so the first request lazily adds it exactly the way `habit.py add SLUG
// NAME` would. A no-op for "no-gluten", which already exists.
func ensureHabit(db *sql.DB, slug, name, startedOn string) error {
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM habits WHERE slug = ?`, slug).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	_, err := db.Exec(`INSERT INTO habits (slug, name, started_on) VALUES (?, ?, ?)`, slug, name, startedOn)
	return err
}

// loadHabitCheckins reads every checkins row (any habit, not just the two
// tracked ones -- the extra rows are simply never looked up) in
// [startDate, endDate] into a slug -> date -> status map.
func loadHabitCheckins(db *sql.DB, startDate, endDate string) (map[string]map[string]string, error) {
	rows, err := db.Query(`
		SELECT h.slug, c.date, c.status
		FROM checkins c
		JOIN habits h ON h.id = c.habit_id
		WHERE c.date >= ? AND c.date <= ?`, startDate, endDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]map[string]string{}
	for rows.Next() {
		var slug, date, status string
		if err := rows.Scan(&slug, &date, &status); err != nil {
			return nil, err
		}
		if out[slug] == nil {
			out[slug] = map[string]string{}
		}
		out[slug][date] = status
	}
	return out, rows.Err()
}

func habitStateFor(checkins map[string]map[string]string, slug, date string) string {
	if s, ok := checkins[slug][date]; ok {
		return s
	}
	return "none"
}

// nextHabitState cycles a habit's per-day state on each click of its
// calendar indicator: no checkin yet -> done -> miss -> back to no checkin.
// This mirrors the three states the page renders (check/cross/dash) and the
// two the checkins.status CHECK constraint allows, plus "no row at all" for
// a day nothing was logged.
func nextHabitState(current string) string {
	switch current {
	case "done":
		return "miss"
	case "miss":
		return "none"
	default: // "" (no row) or anything unrecognized
		return "done"
	}
}

// toggleHabitCheckin advances one habit/date's state by one step (see
// nextHabitState) and persists it: an upsert for done/miss (the same
// ON CONFLICT shape habit.py's cmd_mark uses), or a delete when cycling
// back to "none". Returns the resulting state.
func toggleHabitCheckin(db *sql.DB, slug, date string) (string, error) {
	var habitID int64
	if err := db.QueryRow(`SELECT id FROM habits WHERE slug = ?`, slug).Scan(&habitID); err != nil {
		return "", fmt.Errorf("resolving habit %q: %w", slug, err)
	}

	var current string
	err := db.QueryRow(`SELECT status FROM checkins WHERE habit_id = ? AND date = ?`, habitID, date).Scan(&current)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}

	next := nextHabitState(current)
	if next == "none" {
		_, err = db.Exec(`DELETE FROM checkins WHERE habit_id = ? AND date = ?`, habitID, date)
		return next, err
	}
	_, err = db.Exec(`
		INSERT INTO checkins (habit_id, date, status) VALUES (?, ?, ?)
		ON CONFLICT(habit_id, date) DO UPDATE SET
			status = excluded.status,
			logged_at = datetime('now', 'localtime')`,
		habitID, date, next)
	return next, err
}

// ---------------------------------------------------------------------
// workouts.db -> auto-detected workout completion
// ---------------------------------------------------------------------

// openWorkoutsDB opens ~/dev/health/training/workouts.db READ-ONLY -- unlike
// openHabitsDB, this page never creates or writes to this database, since it
// is owned by the health repo's own Garmin/TrainingPeaks ingestion pipeline
// and this page only auto-detects completed sessions from it. Mirrors the
// file:...?mode=ro pattern internal/brain/harvest_gemini.go already uses for
// another read-only cross-repo SQLite file. Verifies the `workouts` table
// and `workout_day` column are actually queryable (not just that the file
// opens) so a stale/mismatched schema is caught here, at registration time,
// rather than surfacing as a silent "no workouts ever auto-detected".
func openWorkoutsDB(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("no workouts db path configured")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	rows, err := db.Query(`SELECT workout_day FROM workouts LIMIT 0`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("workouts table unavailable: %w", err)
	}
	rows.Close()
	return db, nil
}

// loadWorkoutDates reads the distinct workout_day values (already
// YYYY-MM-DD text, matching habits.db's date format exactly) in
// [startDate, endDate] with at least one logged session, into a date -> true
// set. db may be nil (workouts.db missing/unreadable at registration time),
// in which case this degrades to an empty set -- auto-detection simply
// contributes nothing and the page falls back to manual-only, matching
// loadFoodDiaryTotals/loadTargetCaloriesRest's best-effort contract for the
// other cross-repo reads on this page.
func loadWorkoutDates(db *sql.DB, startDate, endDate string) map[string]bool {
	out := map[string]bool{}
	if db == nil {
		return out
	}
	rows, err := db.Query(`SELECT DISTINCT workout_day FROM workouts WHERE workout_day >= ? AND workout_day <= ?`, startDate, endDate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  workouts db query: %v (auto workout detection skipped for this request)\n", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if rows.Scan(&d) != nil {
			continue
		}
		out[d] = true
	}
	return out
}

// resolveWorkoutState applies Change 1's precedence: an explicit habits.db
// checkin for "workout" (done or miss) always wins, since it is the user
// directly saying what happened that day. Only when no such row exists does
// a completed session in workouts.db count, showing as an automatic "done".
// A day with neither is "none".
func resolveWorkoutState(checkins map[string]map[string]string, workoutDates map[string]bool, date string) (state string, auto bool) {
	if s, ok := checkins["workout"][date]; ok {
		return s, false
	}
	if workoutDates[date] {
		return "done", true
	}
	return "none", false
}

// ---------------------------------------------------------------------
// food_diary.json + menu.yaml -> "eating-correctly" percentage
// ---------------------------------------------------------------------

type foodDiaryEntry struct {
	Date     string  `json:"date"`
	Calories float64 `json:"calories"`
}

// dailyFoodTotal separates "logged something" (HasEntries) from the summed
// calories (Total), so a day with real entries that happen to sum to 0 kcal
// still counts as logged -- only a day with zero food_diary.json rows at
// all is "no data".
type dailyFoodTotal struct {
	Total      float64
	HasEntries bool
}

func loadFoodDiaryTotals(path string) (map[string]*dailyFoodTotal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []foodDiaryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	out := map[string]*dailyFoodTotal{}
	for _, e := range entries {
		d := out[e.Date]
		if d == nil {
			d = &dailyFoodTotal{}
			out[e.Date] = d
		}
		d.Total += e.Calories
		d.HasEntries = true
	}
	return out, nil
}

// loadTargetCaloriesRest reads menu.yaml's targets.calories_rest -- the
// flat daily calorie target used as the denominator below. Reuses
// loadMenuYAML/asMap/asInt from menu_web.go (same package). Returns 0 on
// any load/parse failure, which eatingCorrectlyPct treats as "no data".
func loadTargetCaloriesRest(menuPath string) int {
	top, err := loadMenuYAML(menuPath)
	if err != nil {
		return 0
	}
	return asInt(asMap(top["targets"])["calories_rest"])
}

// eatingCorrectlyPct is the "eating-correctly" heuristic: how much of the
// day's flat calorie target (targets.calories_rest in menu.yaml) was
// actually logged in the food diary that day, capped at 100 so an
// over-target day doesn't score above "fully adherent". This is
// deliberately a calorie-COVERAGE proxy, not a food-quality judgment -- it
// never looks at what was eaten, only whether the day's logged calories
// approach the plan's target. Kept intentionally simple per the page's
// "keep it simple" brief; a fancier version could weight protein or
// penalize overshoot, but this one is easy to reason about and to explain
// out loud. Callers only invoke this when the day has at least one
// food-diary entry; the "no data" case (no entries, or no known target) is
// represented by a nil pointer, never by this function.
func eatingCorrectlyPct(loggedKcal float64, targetKcal int) *int {
	pct := int(math.Round(loggedKcal / float64(targetKcal) * 100))
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	return &pct
}

// ---------------------------------------------------------------------
// habits.db + food diary -> response
// ---------------------------------------------------------------------

// buildHabitsResponse assembles the rolling window ending on refDate's
// calendar date (inclusive), oldest first. The window covers AT LEAST
// habitsWindowDays (14) days, but its start is snapped back to the Sunday of
// the week containing (today - 13 days), so the oldest (bottom) calendar row
// is always a full Sun-Sat week instead of starting mid-week -- the newest
// (top) row staying partial (Sun..today) is expected. foodDiaryPath and
// menuPath are read fresh on every call (no caching), matching
// buildMenuResponse's contract for /api/menu -- and both degrade to "no
// data" rather than failing the request, since they live in a separate
// personal-health repo that may not exist on every machine running
// `aida serve`. workoutsDB is likewise best-effort and may be nil (see
// loadWorkoutDates).
func buildHabitsResponse(db *sql.DB, foodDiaryPath, menuPath string, workoutsDB *sql.DB, refDate time.Time) (habitsResponse, error) {
	loc := refDate.Location()
	today := time.Date(refDate.Year(), refDate.Month(), refDate.Day(), 0, 0, 0, 0, loc)
	minStart := today.AddDate(0, 0, -(habitsWindowDays - 1))
	start := minStart.AddDate(0, 0, -int(minStart.Weekday())) // snap back to that week's Sunday (Weekday(): Sun=0)
	numDays := int(today.Sub(start).Hours()/24) + 1
	todayStr := today.Format("2006-01-02")
	startStr := start.Format("2006-01-02")

	checkins, err := loadHabitCheckins(db, startStr, todayStr)
	if err != nil {
		return habitsResponse{}, fmt.Errorf("loading checkins: %w", err)
	}

	foodTotals, _ := loadFoodDiaryTotals(foodDiaryPath)              // best-effort -- nil map degrades every day to no-data
	targetKcal := loadTargetCaloriesRest(menuPath)                   // 0 degrades every day to no-data
	workoutDates := loadWorkoutDates(workoutsDB, startStr, todayStr) // best-effort -- empty set degrades to manual-only

	days := make([]habitDay, 0, numDays)
	for i := 0; i < numDays; i++ {
		d := start.AddDate(0, 0, i)
		dateStr := d.Format("2006-01-02")

		var kcal int
		var pct *int
		if ft := foodTotals[dateStr]; ft != nil && ft.HasEntries {
			kcal = int(math.Round(ft.Total))
			if targetKcal > 0 {
				pct = eatingCorrectlyPct(ft.Total, targetKcal)
			}
		}

		workoutState, workoutAuto := resolveWorkoutState(checkins, workoutDates, dateStr)

		days = append(days, habitDay{
			Date:        dateStr,
			DowIndex:    int(d.Weekday()),
			IsToday:     dateStr == todayStr,
			Gluten:      habitStateFor(checkins, "no-gluten", dateStr),
			Workout:     workoutState,
			WorkoutAuto: workoutAuto,
			EatingKcal:  kcal,
			EatingPct:   pct,
		})
	}

	return habitsResponse{
		Today:      todayStr,
		RangeStart: startStr,
		RangeEnd:   todayStr,
		TargetKcal: targetKcal,
		Days:       days,
	}, nil
}

// ---------------------------------------------------------------------
// HTTP wiring
// ---------------------------------------------------------------------

// registerHabitsWebRoutes registers the habit-tracking calendar: GET
// /habits and /habits/ (the embedded HTML page), GET /api/habits (the
// rolling calendar dataset, at least 14 days -- see buildHabitsResponse),
// and POST /api/habits/toggle (advance one habit/date's state and persist to
// habits.db). Loopback-only -- unlike the menu page, this is never mounted
// on the LAN listener; it's personal, not for the kids' phones.
//
// The habits.db sqlite handle is opened once, at registration time, and
// held for the daemon's lifetime (matching how the brain/jobs stores are
// opened once in runHTTPDaemon) -- opening it eagerly here, rather than
// per-request, means a bad path is diagnosed once at startup instead of on
// every request, but a failure here does NOT abort `aida serve`: it's
// logged and the two API routes return a clear 500 instead, since
// ~/dev/health may not exist on every machine that runs the daemon.
//
// workoutsDBPath (read-only, see openWorkoutsDB) is opened the same way, but
// its failure mode is softer: it only disables auto-detection of completed
// workouts from workouts.db, never the page itself -- manual toggles keep
// working either way.
func registerHabitsWebRoutes(mux *http.ServeMux, habitsDBPath, foodDiaryPath, menuPath, workoutsDBPath string) {
	htmlHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(habitsWebHTML)
	}
	mux.HandleFunc("GET /habits", htmlHandler)
	mux.HandleFunc("GET /habits/", htmlHandler)

	db, dbErr := openHabitsDB(habitsDBPath)
	if dbErr != nil {
		fmt.Fprintf(os.Stderr, "⚠️  habits db: %v (the /habits page will show an error until this is fixed)\n", dbErr)
	}

	workoutsDB, workoutsDBErr := openWorkoutsDB(workoutsDBPath)
	if workoutsDBErr != nil {
		fmt.Fprintf(os.Stderr, "⚠️  workouts db: %v (workout auto-detection disabled -- manual habits.db toggles still work)\n", workoutsDBErr)
	}

	mux.HandleFunc("GET /api/habits", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if db == nil {
			httpError(w, http.StatusInternalServerError, "habits db unavailable: "+dbErr.Error())
			return
		}

		refDate := time.Now()
		if dateParam := r.URL.Query().Get("date"); dateParam != "" {
			parsed, err := time.ParseInLocation("2006-01-02", dateParam, time.Local)
			if err != nil {
				httpError(w, http.StatusBadRequest, "invalid date: "+err.Error())
				return
			}
			refDate = parsed
		}

		todayStr := refDate.Format("2006-01-02")
		if err := ensureHabit(db, "no-gluten", "No gluten", todayStr); err != nil {
			httpError(w, http.StatusInternalServerError, "ensuring no-gluten habit: "+err.Error())
			return
		}
		if err := ensureHabit(db, "workout", "Workout", todayStr); err != nil {
			httpError(w, http.StatusInternalServerError, "ensuring workout habit: "+err.Error())
			return
		}

		resp, err := buildHabitsResponse(db, foodDiaryPath, menuPath, workoutsDB, refDate)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "building habits response: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("POST /api/habits/toggle", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if db == nil {
			httpError(w, http.StatusInternalServerError, "habits db unavailable: "+dbErr.Error())
			return
		}

		var body struct {
			Slug string `json:"slug"`
			Date string `json:"date"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if !isToggleableHabitSlug(body.Slug) {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("invalid habit %q", body.Slug))
			return
		}
		if _, err := time.Parse("2006-01-02", body.Date); err != nil {
			httpError(w, http.StatusBadRequest, "invalid date: "+err.Error())
			return
		}

		if err := ensureHabit(db, body.Slug, defaultHabitName(body.Slug), body.Date); err != nil {
			httpError(w, http.StatusInternalServerError, "ensuring habit: "+err.Error())
			return
		}
		next, err := toggleHabitCheckin(db, body.Slug, body.Date)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"slug": body.Slug, "date": body.Date, "status": next})
	})
}
