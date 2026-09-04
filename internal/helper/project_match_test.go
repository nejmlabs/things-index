package helper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type captureProjectFixture struct {
	id, title, area string
	status, trashed int
}

func captureProjectDB(t *testing.T, fixtures []captureProjectFixture) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", setupTestThingsDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, fixture := range fixtures {
		if fixture.area != "" {
			if _, err := db.Exec(`INSERT OR IGNORE INTO TMArea (uuid, title) VALUES (?, ?)`, "area-"+fixture.area, fixture.area); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, area, status, trashed) VALUES (?, 1, ?, ?, ?, ?)`,
			fixture.id, fixture.title, "area-"+fixture.area, fixture.status, fixture.trashed); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestResolveCaptureProjectMatching(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, requested, id, want string
		projects                  []captureProjectFixture
	}{
		{
			name: "case insensitive exact beats partial", requested: "KITCHEN", want: "exact",
			projects: []captureProjectFixture{{id: "partial", title: "Kitchen renovation"}, {id: "exact", title: "Kitchen"}},
		},
		{
			name: "exact beats normalized alternative", requested: "Kitchen renovation", want: "exact",
			projects: []captureProjectFixture{{id: "normalized", title: "Kitchen-renovation"}, {id: "exact", title: "Kitchen renovation"}},
		},
		{
			name: "punctuation whitespace and case", requested: "  KITCHEN /\t renovation  ", want: "kitchen",
			projects: []captureProjectFixture{{id: "kitchen", title: "Kitchen—Renovation"}},
		},
		{
			name: "whole word fragment", requested: "kitchen", want: "kitchen",
			projects: []captureProjectFixture{{id: "kitchen", title: "Kitchen renovation", area: "Home"}},
		},
		{
			name: "missing letter", requested: "Kichen renovation", want: "kitchen",
			projects: []captureProjectFixture{{id: "kitchen", title: "Kitchen renovation"}},
		},
		{
			name: "adjacent transposition", requested: "Shoppign", want: "shopping",
			projects: []captureProjectFixture{{id: "shopping", title: "Shopping"}},
		},
		{
			name: "transposition plus missing letter in long name", requested: "Kithcen renvation", want: "kitchen",
			projects: []captureProjectFixture{{id: "kitchen", title: "Kitchen renovation"}},
		},
		{
			name: "three letter whole word", requested: "car", want: "car",
			projects: []captureProjectFixture{{id: "car", title: "Car repairs"}},
		},
		{
			name: "three letter whole word tax", requested: "tax", want: "tax",
			projects: []captureProjectFixture{{id: "tax", title: "Tax return"}},
		},
		{
			name: "reordered words", requested: "renovation kitchen", want: "kitchen",
			projects: []captureProjectFixture{{id: "kitchen", title: "Kitchen renovation"}},
		},
		{
			name: "reordered with typo", requested: "renovation kichen", want: "kitchen",
			projects: []captureProjectFixture{{id: "kitchen", title: "Kitchen renovation"}},
		},
		{
			name: "reordered beats partial", requested: "renovation kitchen", want: "kitchen",
			projects: []captureProjectFixture{{id: "other", title: "Kitchen renovation planning"}, {id: "kitchen", title: "Kitchen renovation"}},
		},
		{
			name: "unicode exact case", requested: "ÉTÉ À PARIS", want: "paris",
			projects: []captureProjectFixture{{id: "paris", title: "Été à Paris"}},
		},
		{
			name: "unicode punctuation", requested: "庭園 / 改修", want: "garden",
			projects: []captureProjectFixture{{id: "garden", title: "庭園—改修"}},
		},
		{
			name: "unicode fragment", requested: "cuisine", want: "kitchen",
			projects: []captureProjectFixture{{id: "kitchen", title: "Rénover la cuisine"}},
		},
		{
			name: "short exact name remains valid", requested: "AI", want: "ai",
			projects: []captureProjectFixture{{id: "ai", title: "AI"}},
		},
		{
			name: "ID disambiguates shared titles and wins over name", requested: "incorrect name", id: "work", want: "work",
			projects: []captureProjectFixture{{id: "home", title: "Planning", area: "Home"}, {id: "work", title: "Planning", area: "Work"}},
		},
		{
			name: "archived exact does not hide active partial", requested: "Kitchen", want: "active",
			projects: []captureProjectFixture{{id: "old", title: "Kitchen", status: 3}, {id: "active", title: "Kitchen renovation"}},
		},
		{
			name: "archived duplicates are ignored", requested: "Planning", want: "active",
			projects: []captureProjectFixture{{id: "old", title: "Planning", status: 3}, {id: "trashed", title: "Planning", trashed: 1}, {id: "active", title: "Planning"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := captureProjectDB(t, tt.projects)
			got, err := resolveCaptureProject(context.Background(), db, tt.requested, tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != tt.want {
				t.Fatalf("matched %#v, want ID %q", got, tt.want)
			}
			for _, project := range tt.projects {
				if project.id == tt.want && (got.Title != project.title || got.Area != project.area) {
					t.Fatalf("canonical project details = %#v, want %#v", got, project)
				}
			}
		})
	}
}

func TestResolveCaptureProjectNeedsClarification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, requested, id, code string
		projects                  []captureProjectFixture
		candidates                int
	}{
		{
			name: "duplicate exact titles in separate areas", requested: "Planning", code: "project_ambiguous", candidates: 2,
			projects: []captureProjectFixture{{id: "work", title: "Planning", area: "Work"}, {id: "home", title: "Planning", area: "Home"}},
		},
		{
			name: "duplicate title case variants", requested: "planning", code: "project_ambiguous", candidates: 2,
			projects: []captureProjectFixture{{id: "a", title: "Planning"}, {id: "b", title: "PLANNING"}},
		},
		{
			name: "normalization collision", requested: "Kitchen / renovation", code: "project_ambiguous", candidates: 2,
			projects: []captureProjectFixture{{id: "a", title: "Kitchen renovation"}, {id: "b", title: "Kitchen-renovation"}},
		},
		{
			name: "close partial alternatives", requested: "kitchen", code: "project_ambiguous", candidates: 2,
			projects: []captureProjectFixture{{id: "a", title: "Kitchen renovation"}, {id: "b", title: "Kitchen planning"}},
		},
		{
			name: "close typo alternatives are not arbitrarily ranked", requested: "Kichen renovation", code: "project_ambiguous", candidates: 2,
			projects: []captureProjectFixture{{id: "a", title: "Kitchen renovation"}, {id: "b", title: "Kitchen renovations"}},
		},
		{
			name: "nonsense", requested: "xyzzy nonsense", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Kitchen renovation"}},
		},
		{
			name: "no substring fragments", requested: "kitch", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Kitchen renovation"}},
		},
		{
			name: "very short fragment", requested: "AI", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "AI development"}},
		},
		{
			name: "short typo is weak", requested: "tax", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Tag"}},
		},
		{
			name: "generic word is weak", requested: "project", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Kitchen project"}},
		},
		{
			name: "bare year is weak", requested: "2026", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Taxes 2026"}},
		},
		{
			name: "different year is not a typo", requested: "Migration 2025", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Migration 2026"}},
		},
		{
			name: "different number in word is not a typo", requested: "Client123 launch", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Client124 launch"}},
		},
		{
			name: "empty request", requested: " \t", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Kitchen renovation"}},
		},
		{
			name: "punctuation only", requested: "---", code: "project_not_found",
			projects: []captureProjectFixture{{id: "a", title: "Kitchen renovation"}},
		},
		{
			name: "no projects", requested: "kitchen", code: "project_not_found",
		},
		{
			name: "completed cancelled and trashed excluded", requested: "Planning", code: "project_not_found",
			projects: []captureProjectFixture{{id: "completed", title: "Planning", status: 3}, {id: "cancelled", title: "Planning", status: 2}, {id: "trashed", title: "Planning", trashed: 1}},
		},
		{
			name: "inactive ID cannot fall back to name", requested: "Planning", id: "completed", code: "project_not_found",
			projects: []captureProjectFixture{{id: "completed", title: "Planning", status: 3}, {id: "active", title: "Planning"}},
		},
		{
			name: "unknown ID cannot fall back to name", requested: "Planning", id: "missing", code: "project_not_found",
			projects: []captureProjectFixture{{id: "active", title: "Planning"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := captureProjectDB(t, tt.projects)
			got, err := resolveCaptureProject(context.Background(), db, tt.requested, tt.id)
			var matchErr *ProjectMatchError
			if !errors.As(err, &matchErr) || matchErr.Code != tt.code {
				t.Fatalf("got project %#v, error %v; want %s", got, err, tt.code)
			}
			if got != (projectMatch{}) || len(matchErr.Candidates) != tt.candidates {
				t.Fatalf("got project %#v, candidates %#v", got, matchErr.Candidates)
			}
			requested := strings.TrimSpace(tt.requested)
			if tt.id != "" {
				requested = tt.id
			}
			if matchErr.Requested != requested {
				t.Fatalf("requested = %q, want %q", matchErr.Requested, requested)
			}
			for _, candidate := range matchErr.Candidates {
				if !strings.Contains(err.Error(), candidate.Title) || !strings.Contains(err.Error(), candidate.ID) || !strings.Contains(err.Error(), candidate.Area) {
					t.Fatalf("error %q omits candidate %#v", err, candidate)
				}
			}
		})
	}
}

func TestResolveCaptureProjectCandidatesAreBoundedAndDeterministic(t *testing.T) {
	t.Parallel()
	var fixtures []captureProjectFixture
	for i := 8; i >= 0; i-- {
		fixtures = append(fixtures, captureProjectFixture{id: fmt.Sprintf("p%d", i), title: "Planning", area: fmt.Sprintf("Area %d", i)})
	}
	db := captureProjectDB(t, fixtures)
	_, err := resolveCaptureProject(context.Background(), db, "planning", "")
	var matchErr *ProjectMatchError
	if !errors.As(err, &matchErr) || matchErr.Code != "project_ambiguous" {
		t.Fatalf("error = %v", err)
	}
	var want []ProjectCandidate
	for i := 0; i < 5; i++ {
		want = append(want, ProjectCandidate{ID: fmt.Sprintf("p%d", i), Title: "Planning", Area: fmt.Sprintf("Area %d", i)})
	}
	if !reflect.DeepEqual(matchErr.Candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", matchErr.Candidates, want)
	}
}

func TestResolveCaptureProjectRejectsTaskID(t *testing.T) {
	t.Parallel()
	db := captureProjectDB(t, nil)
	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title) VALUES ('task', 0, 'Planning')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "task"} {
		_, err := resolveCaptureProject(context.Background(), db, "Planning", id)
		var matchErr *ProjectMatchError
		if !errors.As(err, &matchErr) || matchErr.Code != "project_not_found" {
			t.Fatalf("ID %q error = %v", id, err)
		}
	}
}

func TestResolveCaptureProjectPropagatesDatabaseFailure(t *testing.T) {
	t.Parallel()
	db := captureProjectDB(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolveCaptureProject(ctx, db, "Kitchen", "")
	var matchErr *ProjectMatchError
	if !errors.Is(err, context.Canceled) || errors.As(err, &matchErr) {
		t.Fatalf("database failure must not become a clarification error: %v", err)
	}
}
