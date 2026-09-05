package capture

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type CreateAreaRequest struct {
	Title          string `json:"title" jsonschema:"Required area name. An existing unique exact name (ignoring case) is reused with a warning; ambiguous names fail without creating an area."`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional stable key for this command. Reuse the same key and unchanged request when retrying."`
}

func (r CreateAreaRequest) Validate() error {
	if err := ValidateIdempotencyKey(r.IdempotencyKey); err != nil {
		return err
	}
	return validateOrganizationName("area title", r.Title)
}

type CreateTagRequest struct {
	Title          string `json:"title" jsonschema:"Required tag name. An existing unique exact name (ignoring case) is reused only under the requested parent, with a warning. Never changes an existing tag's parent."`
	Parent         string `json:"parent,omitempty" jsonschema:"Optional exact name of one existing parent tag (ignoring case). Missing or ambiguous parents fail. Omit to create a top-level tag."`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional stable key for this command. Reuse the same key and unchanged request when retrying."`
}

func (r CreateTagRequest) Validate() error {
	if err := ValidateIdempotencyKey(r.IdempotencyKey); err != nil {
		return err
	}
	if err := validateOrganizationName("tag title", r.Title); err != nil {
		return err
	}
	// The rest of the Things bridge applies tags through a comma-separated
	// native field, so a comma would create a tag it cannot address safely.
	if strings.Contains(r.Title, ",") {
		return errors.New("tag title must not contain a comma")
	}
	if r.Parent == "" {
		return nil
	}
	if err := validateOrganizationName("parent tag", r.Parent); err != nil {
		return err
	}
	if strings.EqualFold(r.Title, r.Parent) {
		return errors.New("a tag cannot be its own parent")
	}
	return nil
}

func validateOrganizationName(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not have leading or trailing whitespace", label)
	}
	if !utf8.ValidString(value) || len(value) > MaxDestinationLen {
		return fmt.Errorf("%s must be valid UTF-8 and at most %d bytes", label, MaxDestinationLen)
	}
	if strings.ContainsFunc(value, unicode.IsControl) || strings.ContainsAny(value, "\u2028\u2029") {
		return fmt.Errorf("%s must be a single line without control characters", label)
	}
	return nil
}
