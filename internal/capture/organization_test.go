package capture

import (
	"strings"
	"testing"
)

func TestOrganizationRequestValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  interface{ Validate() error }
		ok   bool
	}{
		{"area", CreateAreaRequest{Title: "Home 🏡"}, true},
		{"area punctuation", CreateAreaRequest{Title: `Home, "Garden" & Work`}, true},
		{"nested tag", CreateTagRequest{Title: "Garden", Parent: "Home"}, true},
		{"top-level tag", CreateTagRequest{Title: "Garden"}, true},
		{"blank", CreateAreaRequest{Title: " "}, false},
		{"trimmed", CreateAreaRequest{Title: " Home"}, false},
		{"control", CreateAreaRequest{Title: "Home\x00Office"}, false},
		{"line break", CreateTagRequest{Title: "Home\nOffice"}, false},
		{"unicode line break", CreateTagRequest{Title: "Home\u2028Office"}, false},
		{"invalid UTF8", CreateAreaRequest{Title: string([]byte{0xff})}, false},
		{"too long", CreateTagRequest{Title: strings.Repeat("a", MaxDestinationLen+1)}, false},
		{"max length", CreateAreaRequest{Title: strings.Repeat("a", MaxDestinationLen)}, true},
		{"comma tag", CreateTagRequest{Title: "Home,Work"}, false},
		{"blank parent", CreateTagRequest{Title: "Garden", Parent: " "}, false},
		{"invalid parent", CreateTagRequest{Title: "Garden", Parent: "Home\tOffice"}, false},
		{"self parent", CreateTagRequest{Title: "Home", Parent: "home"}, false},
		{"area key", CreateAreaRequest{Title: "Home", IdempotencyKey: "create-home"}, true},
		{"invalid area key", CreateAreaRequest{Title: "Home", IdempotencyKey: "create\nhome"}, false},
		{"invalid tag key", CreateTagRequest{Title: "Home", IdempotencyKey: strings.Repeat("a", 129)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want valid=%t", err, tc.ok)
			}
		})
	}
}
