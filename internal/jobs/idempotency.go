package jobs

import (
	"errors"
	"regexp"
)

var (
	ErrInvalidIdempotencyKey = errors.New("Idempotency-Key must contain 1-128 ASCII letters, digits, dots, underscores, colons or hyphens")
	ErrIdempotencyConflict   = errors.New("Idempotency-Key was already used for a different submission")
	idempotencyKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// ValidateIdempotencyKey validates a supplied key; an absent header is handled
// separately. Keys are opaque, case-sensitive and never trimmed or rewritten.
func ValidateIdempotencyKey(key string) error {
	if !idempotencyKeyPattern.MatchString(key) {
		return ErrInvalidIdempotencyKey
	}
	return nil
}
