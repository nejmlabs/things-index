package capture

import "testing"

func TestRequestValidate(t *testing.T) {
	t.Parallel()

	valid := Request{
		Title:       "Buy milk",
		Destination: &Destination{Kind: DestinationProject, Name: "Shopping", Heading: "Groceries"},
		Schedule:    &Schedule{Start: StartOnDate, Date: "2026-08-17", ReminderAt: "2026-08-17T08:15:00+01:00"},
		Deadline:    "2026-08-18",
		Tags:        []string{"Errand"},
		Checklist:   []string{"Check fridge"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestRequestValidateRejectsHeadingOutsideProject(t *testing.T) {
	t.Parallel()

	request := Request{Title: "Buy milk", Destination: &Destination{Kind: DestinationArea, Name: "Home", Heading: "Groceries"}}
	if err := request.Validate(); err == nil {
		t.Fatal("expected an area heading to be rejected")
	}
}

func TestRequestValidateRejectsInconsistentSchedule(t *testing.T) {
	t.Parallel()

	request := Request{Title: "Buy milk", Schedule: &Schedule{Start: StartAnytime, Date: "2026-08-17"}}
	if err := request.Validate(); err == nil {
		t.Fatal("expected inconsistent schedule to be rejected")
	}
}

func TestRequestValidateRejectsEveningWithoutDate(t *testing.T) {
	t.Parallel()

	request := Request{Title: "Buy milk", Schedule: &Schedule{Start: StartAnytime, Evening: true}}
	if err := request.Validate(); err == nil {
		t.Fatal("expected evening without an on-date schedule to be rejected")
	}
}

func TestRequestValidateRejectsMultilineChecklistItem(t *testing.T) {
	t.Parallel()

	request := Request{Title: "Buy milk", Checklist: []string{"First line\nSecond line"}}
	if err := request.Validate(); err == nil {
		t.Fatal("expected a multiline checklist item to be rejected")
	}
}

func TestRequestHashIsStable(t *testing.T) {
	t.Parallel()

	request := Request{Title: "Buy milk", Tags: []string{"Errand"}}
	first, err := request.Hash()
	if err != nil {
		t.Fatal(err)
	}
	second, err := request.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("hash changed: %q != %q", first, second)
	}
}
