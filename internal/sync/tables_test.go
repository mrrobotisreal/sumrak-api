package sync

import (
	"sort"
	"strings"
	"testing"
)

// Keep in lockstep with the app registry test (T104).
const pinned = "achievements,analytics_events,assessments,bank_items,bookmarks,cards,checkpoint_results,course_attempts,course_progress,daily_activity,daily_quests,dialogue_endings_seen,dialogue_runs,encounters,exam_attempts,exam_item_cards,exam_responses,frozen_days,game_sessions,grammar_lessons,journal_entries,leech_dismissals,notes,review_log,scenario_attempts,scenario_runs,settings,story_progress,unit_progress,word_profiles"

func TestTablesPinned(t *testing.T) {
	got := append([]string(nil), Tables...)
	sort.Strings(got)
	if strings.Join(got, ",") != pinned {
		t.Fatalf("allowlist drifted:\n got %s\nwant %s", strings.Join(got, ","), pinned)
	}
	if len(got) != 30 {
		t.Fatalf("len = %d", len(got))
	}
	for _, bad := range []string{"sync_outbox", "sync_state", "imported_packs", "records", ""} {
		if Allowed(bad) {
			t.Fatalf("%q must not be allowed", bad)
		}
	}
}
