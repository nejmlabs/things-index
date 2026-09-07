package capture

import (
	"strings"
	"testing"
)

func TestRequestRejectsMixedOperations(t *testing.T) {
	for _, request := range []Request{
		{CreateProjectRequest: &CreateProjectRequest{Title: "Work"}, CreateTagRequest: &CreateTagRequest{Title: "Urgent"}},
		{TaskFields: TaskFields{Title: "Capture"}, ArchiveTaskRequest: &ArchiveTaskRequest{ID: "id"}},
		{TaskFields: TaskFields{Notes: "ignored"}, QueryTasksRequest: &QueryTasksRequest{}},
		{HeadingRequest: &HeadingRequest{Project: "Work", Heading: "Next"}},
		{HeadingOperation: "unknown", HeadingRequest: &HeadingRequest{Project: "Work", Heading: "Next"}},
		{HeadingOperation: "rename", HeadingRequest: &HeadingRequest{Project: "Work", Heading: "Next"}},
		{HeadingOperation: "create"},
	} {
		if err := request.Validate(); err == nil {
			t.Fatalf("accepted mixed/incomplete request: %#v", request)
		}
	}
}

func TestAllWriteKeysAreValidated(t *testing.T) {
	bad := "key\nsecond line"
	for _, request := range []Request{
		{TaskFields: TaskFields{Title: "Capture", IdempotencyKey: bad}},
		{CreateProjectRequest: &CreateProjectRequest{Title: "Work", IdempotencyKey: bad}},
		{UpdateTaskRequest: &UpdateTaskRequest{ID: "id", NewTitle: "New", IdempotencyKey: bad}},
		{ArchiveTaskRequest: &ArchiveTaskRequest{ID: "id", IdempotencyKey: bad}},
		{ArchiveProjectRequest: &ArchiveProjectRequest{ID: "id", IdempotencyKey: bad}},
		{HeadingOperation: "create", HeadingRequest: &HeadingRequest{Project: "Work", Heading: "Next", IdempotencyKey: bad}},
		{CreateAreaRequest: &CreateAreaRequest{Title: "Work", IdempotencyKey: bad}},
		{CreateTagRequest: &CreateTagRequest{Title: "Urgent", IdempotencyKey: bad}},
		{TaskFields: TaskFields{IdempotencyKey: "outer"}, CreateTagRequest: &CreateTagRequest{Title: "Urgent", IdempotencyKey: "inner"}},
	} {
		if err := request.Validate(); err == nil {
			t.Fatalf("accepted invalid key: %#v", request)
		}
	}
}

func TestProjectFieldsValidatedBeforeQueueing(t *testing.T) {
	valid := CreateProjectRequest{Title: "Work", Area: "Business", When: "2026-09-05", Deadline: "2026-09-10", Tags: []string{"Urgent"}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CreateProjectRequest){
		"blank title":      func(r *CreateProjectRequest) { r.Title = " " },
		"multiline title":  func(r *CreateProjectRequest) { r.Title = "Work\nHome" },
		"long title":       func(r *CreateProjectRequest) { r.Title = strings.Repeat("a", MaxTitleBytes+1) },
		"invalid date":     func(r *CreateProjectRequest) { r.When = "2026-02-30" },
		"invalid deadline": func(r *CreateProjectRequest) { r.Deadline = "tomorrow" },
		"large notes":      func(r *CreateProjectRequest) { r.Notes = strings.Repeat("a", MaxNotesBytes+1) },
		"comma tag":        func(r *CreateProjectRequest) { r.Tags = []string{"one,two"} },
		"NUL tag":          func(r *CreateProjectRequest) { r.Tags = []string{"one\x00two"} },
	} {
		t.Run(name, func(t *testing.T) {
			req := valid
			mutate(&req)
			if req.Validate() == nil {
				t.Fatal("accepted invalid project fields")
			}
		})
	}
}
