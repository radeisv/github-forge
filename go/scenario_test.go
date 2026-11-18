package main

import "testing"

func TestScenarioPort(t *testing.T) {
	s := buildScenario()
	if s.viewer.login != "adaiversen" {
		t.Fatalf("viewer login = %q, want adaiversen", s.viewer.login)
	}
	if s.viewer.id != 1000 || s.viewer.email != "adaiversen@example.invalid" {
		t.Fatalf("viewer = %+v", s.viewer)
	}
	if s.stranger.login != "olgabauer" {
		t.Fatalf("stranger login = %q, want olgabauer", s.stranger.login)
	}
	if s.teammate.login != "quinnkeller" {
		t.Fatalf("teammate login = %q, want quinnkeller", s.teammate.login)
	}
	if s.org != "faint-meadow-org" {
		t.Fatalf("org = %q, want faint-meadow-org", s.org)
	}
	if s.repos[0].name != "amber-thistle" {
		t.Fatalf("repos[0].name = %q, want amber-thistle", s.repos[0].name)
	}
	if s.repos[29].name != "amber-badger" {
		t.Fatalf("repos[29].name = %q, want amber-badger", s.repos[29].name)
	}
	wantBranches := []string{"main", "develop", "release/1.0", "docs/graphql-cursor-pagination", "feature/issue-label-validation", "feature/push-report-status-checks", "fix/repo-pagination", "perf/issue-label-validation", "docs/repo-pagination", "perf/conditional-requests-2", "fix/shallow-fetch-support"}
	if len(s.branches) != len(wantBranches) {
		t.Fatalf("branches = %v", s.branches)
	}
	for i, b := range wantBranches {
		if s.branches[i] != b {
			t.Fatalf("branches[%d] = %q, want %q (full: %v)", i, s.branches[i], b, s.branches)
		}
	}
}
