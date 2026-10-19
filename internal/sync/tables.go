package sync

// Tables is the allowlist of synced SQLite tables (snake_case, the app's
// names; design ACCOUNTS_AND_SYNC §5.2). Device-local tables (sync_state,
// imported_packs, sync_outbox, ...) are deliberately absent.
//
// PINNED: tables_test.go pins this exact list. T104's app registry test
// (src/features/account-sync/registry.ts) MUST match it — change both together.
var Tables = []string{
	// append-only
	"review_log", "encounters", "game_sessions", "analytics_events",
	"scenario_attempts", "exam_responses", "course_attempts",
	"checkpoint_results", "assessments",
	// FSRS state
	"cards", "exam_item_cards",
	// monotonic
	"story_progress", "unit_progress", "course_progress", "achievements",
	"frozen_days", "dialogue_endings_seen", "leech_dismissals",
	// LWW row
	"bank_items", "journal_entries", "notes", "bookmarks", "dialogue_runs",
	"scenario_runs", "exam_attempts", "word_profiles", "grammar_lessons",
	"daily_quests",
	// per-device + settings
	"daily_activity", "settings",
}

var tableSet = func() map[string]bool {
	m := make(map[string]bool, len(Tables))
	for _, t := range Tables {
		m[t] = true
	}
	return m
}()

// Allowed reports whether tbl may be synced.
func Allowed(tbl string) bool { return tableSet[tbl] }
