package capture

import (
	"errors"
	"strings"
	"unicode/utf8"
)

func ValidateIdempotencyKey(key string) error {
	if !utf8.ValidString(key) || len(key) > 128 || strings.ContainsAny(key, "\r\n\x00") {
		return errors.New("idempotency_key must be valid UTF-8, at most 128 bytes, and a single line without NUL")
	}
	return nil
}

// OperationKey supports both public write inputs and the internal job envelope.
func (r Request) OperationKey() string {
	switch {
	case r.HeadingRequest != nil:
		return r.HeadingRequest.IdempotencyKey
	case r.ArchiveTaskRequest != nil:
		return r.ArchiveTaskRequest.IdempotencyKey
	case r.ArchiveProjectRequest != nil:
		return r.ArchiveProjectRequest.IdempotencyKey
	case r.CreateProjectRequest != nil:
		return r.CreateProjectRequest.IdempotencyKey
	case r.UpdateTaskRequest != nil:
		return r.UpdateTaskRequest.IdempotencyKey
	case r.CreateAreaRequest != nil:
		return r.CreateAreaRequest.IdempotencyKey
	case r.CreateTagRequest != nil:
		return r.CreateTagRequest.IdempotencyKey
	default:
		return r.IdempotencyKey
	}
}
