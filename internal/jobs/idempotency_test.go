package jobs

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateIdempotencyKey(t *testing.T) {
	for _, tt := range []struct {
		name  string
		key   string
		valid bool
	}{
		{"UUID", "67b001de-ddb4-4c3d-98e4-2fc9466de033", true},
		{"allowed characters", "Campaign_42:migration.v1-A", true},
		{"one character", "a", true},
		{"maximum length", strings.Repeat("k", 128), true},
		{"too long", strings.Repeat("k", 129), false},
		{"empty supplied key", "", false},
		{"leading space", " key", false},
		{"trailing space", "key ", false},
		{"internal space", "key one", false},
		{"comma joined", "one,two", false},
		{"quoted", `"key"`, false},
		{"non ASCII", "你好", false},
		{"newline", "key\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIdempotencyKey(tt.key)
			if tt.valid && err != nil {
				t.Fatal(err)
			}
			if !tt.valid && !errors.Is(err, ErrInvalidIdempotencyKey) {
				t.Fatalf("error = %v, want invalid key", err)
			}
		})
	}
}
