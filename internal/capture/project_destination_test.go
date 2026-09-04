package capture

import (
	"strings"
	"testing"
)

func TestProjectDestinationValidation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		destination Destination
		valid       bool
	}{
		{"spoken name", Destination{Kind: DestinationProject, Name: "kichen renovation"}, true},
		{"id only", Destination{Kind: DestinationProject, ID: "project-1"}, true},
		{"id and heading", Destination{Kind: DestinationProject, ID: "project-1", Heading: "Supplies"}, true},
		{"id and name", Destination{Kind: DestinationProject, ID: "project-1", Name: "Kitchen"}, true},
		{"missing identity", Destination{Kind: DestinationProject}, false},
		{"blank id", Destination{Kind: DestinationProject, ID: " "}, false},
		{"multiline id", Destination{Kind: DestinationProject, ID: "project\n1"}, false},
		{"long id", Destination{Kind: DestinationProject, ID: strings.Repeat("x", MaxDestinationLen+1)}, false},
		{"inbox id", Destination{Kind: DestinationInbox, ID: "project-1"}, false},
		{"area id", Destination{Kind: DestinationArea, Name: "Home", ID: "area-1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := Request{TaskFields: TaskFields{Title: "Buy paint", Destination: &tc.destination}}
			if err := request.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
