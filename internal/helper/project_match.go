package helper

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/nejmlabs/things-index/internal/capture"
)

// ProjectCandidate identifies a project without relying on its possibly shared
// title. Area distinguishes candidates in diagnostic errors.
type ProjectCandidate struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Area  string `json:"area,omitempty"`
}

// ProjectMatchError means a capture destination could not be resolved uniquely.
// Capture handles this by saving to Inbox with the requested placement in notes.
type ProjectMatchError struct {
	Code       string             `json:"code"`
	Requested  string             `json:"requested"`
	Candidates []ProjectCandidate `json:"candidates,omitempty"`
}

func (e *ProjectMatchError) Error() string {
	if e.Code != "project_ambiguous" {
		return fmt.Sprintf("no active project matches %q", e.Requested)
	}
	choices := make([]string, 0, len(e.Candidates))
	for _, candidate := range e.Candidates {
		choice := fmt.Sprintf("%q", candidate.Title)
		if candidate.Area != "" {
			choice += fmt.Sprintf(" in %q", candidate.Area)
		}
		choices = append(choices, fmt.Sprintf("%s (ID: %s)", choice, candidate.ID))
	}
	return fmt.Sprintf("project %q matches multiple active projects: %s", e.Requested, strings.Join(choices, "; "))
}

func projectInboxWarning(destination capture.Destination, matchErr *ProjectMatchError) string {
	label := fmt.Sprintf("%q", destination.Name)
	if destination.ID != "" {
		if destination.Name == "" {
			label = fmt.Sprintf("ID %q", destination.ID)
		} else {
			label += fmt.Sprintf(" (ID: %s)", destination.ID)
		}
	}
	reason := "did not match an active project"
	if matchErr.Code == "project_ambiguous" {
		reason = "matched multiple active projects"
	}
	warning := fmt.Sprintf("ThingsIndex warning: requested project %s %s; captured in Inbox.", label, reason)
	if destination.Heading != "" {
		warning += fmt.Sprintf(" Requested heading: %q.", destination.Heading)
	}
	return warning
}

type projectMatch struct {
	ID    string
	Title string
	Area  string
}

// resolveCaptureProject is intentionally limited to selecting the destination
// of a new task. Editing/deleting projects must not acquire fuzzy semantics.
func resolveCaptureProject(ctx context.Context, db *sql.DB, name, id string) (projectMatch, error) {
	name, id = strings.TrimSpace(name), strings.TrimSpace(id)
	query := `SELECT p.uuid, COALESCE(p.title, ''), COALESCE(a.title, '')
		FROM TMTask p LEFT JOIN TMArea a ON p.area = a.uuid
		WHERE p.type = 1 AND p.status = 0 AND p.trashed = 0`
	var args []any
	if id != "" {
		query += " AND p.uuid = ?"
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return projectMatch{}, fmt.Errorf("read capture projects: %w", err)
	}
	defer rows.Close()
	var projects []projectMatch
	for rows.Next() {
		var project projectMatch
		if err := rows.Scan(&project.ID, &project.Title, &project.Area); err != nil {
			return projectMatch{}, fmt.Errorf("read capture project: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return projectMatch{}, fmt.Errorf("read capture projects: %w", err)
	}
	if id != "" {
		if len(projects) == 1 {
			return projects[0], nil
		}
		return projectMatch{}, &ProjectMatchError{Code: "project_not_found", Requested: id}
	}

	// Keep precedence explicit: a real title must beat a fuzzy alternative.
	var exact []projectMatch
	for _, project := range projects {
		if name != "" && strings.EqualFold(name, strings.TrimSpace(project.Title)) {
			exact = append(exact, project)
		}
	}
	if len(exact) != 0 {
		return chooseCaptureProject(name, exact)
	}

	normalized := normalizeProjectName(name)
	if normalized == "" {
		return chooseCaptureProject(name, nil)
	}
	var normalizedMatches, reordered, partial, typos []projectMatch
	words := strings.Fields(normalized)
	sortedName := sortedProjectWords(words)
	meaningful := meaningfulProjectWords(words)
	for _, project := range projects {
		title := normalizeProjectName(project.Title)
		titleWords := strings.Fields(title)
		if normalized == title {
			normalizedMatches = append(normalizedMatches, project)
			continue
		}
		if sortedName == sortedProjectWords(titleWords) {
			reordered = append(reordered, project)
			continue
		}
		if meaningful && containsProjectWords(titleWords, words) {
			partial = append(partial, project)
			continue
		}
		// Comparing sorted words also handles a typo in a reordered full name.
		if projectNamesWithinTypoDistance(sortedName, sortedProjectWords(titleWords)) {
			typos = append(typos, project)
		}
	}
	for _, matches := range [][]projectMatch{normalizedMatches, reordered, partial, typos} {
		if len(matches) != 0 {
			return chooseCaptureProject(name, matches)
		}
	}
	return chooseCaptureProject(name, nil)
}

func chooseCaptureProject(requested string, projects []projectMatch) (projectMatch, error) {
	if len(projects) == 1 {
		return projects[0], nil
	}
	err := &ProjectMatchError{Code: "project_not_found", Requested: requested}
	if len(projects) == 0 {
		return projectMatch{}, err
	}
	err.Code = "project_ambiguous"
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Title != projects[j].Title {
			return projects[i].Title < projects[j].Title
		}
		if projects[i].Area != projects[j].Area {
			return projects[i].Area < projects[j].Area
		}
		return projects[i].ID < projects[j].ID
	})
	const maxCandidates = 5
	if len(projects) > maxCandidates {
		projects = projects[:maxCandidates]
	}
	for _, project := range projects {
		err.Candidates = append(err.Candidates, ProjectCandidate(project))
	}
	return projectMatch{}, err
}

func normalizeProjectName(name string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

func sortedProjectWords(words []string) string {
	words = append([]string(nil), words...)
	sort.Strings(words)
	return strings.Join(words, " ")
}

// Require a useful word before accepting an incomplete title. Short fragments,
// bare years, and generic words such as "project" must not route a task.
func meaningfulProjectWords(words []string) bool {
	for _, word := range words {
		switch word {
		case "the", "and", "for", "with", "project", "projects":
			continue
		}
		letters := 0
		for _, r := range word {
			if unicode.IsLetter(r) {
				letters++
			}
		}
		if letters >= 3 {
			return true
		}
	}
	return false
}

func containsProjectWords(title, requested []string) bool {
	counts := make(map[string]int, len(title))
	for _, word := range title {
		counts[word]++
	}
	for _, word := range requested {
		if counts[word] == 0 {
			return false
		}
		counts[word]--
	}
	return true
}

func projectNamesWithinTypoDistance(requested, title string) bool {
	a, b := []rune(requested), []rune(title)
	// Full names only, with one edit for ordinary names or two for long names.
	// Bound work even if a database contains an exceptionally long title.
	if len(a) < 6 || len(b) < 6 || len(a) > 256 || len(b) > 256 {
		return false
	}
	// Numbers usually distinguish versions, years, or clients. A near spelling
	// must never silently change those identifiers.
	if projectNameNumbers(requested) != projectNameNumbers(title) {
		return false
	}
	limit := 1
	if len(a) >= 12 && len(b) >= 12 {
		limit = 2
	}
	if len(a)-len(b) > limit || len(b)-len(a) > limit {
		return false
	}
	beforePrevious := make([]int, len(b)+1)
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i, ra := range a {
		current[0] = i + 1
		rowMinimum := current[0]
		for j, rb := range b {
			cost := 0
			if ra != rb {
				cost = 1
			}
			current[j+1] = min(current[j]+1, previous[j+1]+1, previous[j]+cost)
			if i > 0 && j > 0 && ra == b[j-1] && a[i-1] == rb {
				// Adjacent transposition is one typo, as in "shoppign".
				current[j+1] = min(current[j+1], beforePrevious[j-1]+1)
			}
			rowMinimum = min(rowMinimum, current[j+1])
		}
		if rowMinimum > limit {
			return false
		}
		beforePrevious, previous, current = previous, current, beforePrevious
	}
	return previous[len(b)] <= limit
}

func projectNameNumbers(name string) string {
	return strings.Join(strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsDigit(r) }), " ")
}
