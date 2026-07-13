package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// newUUIDv7 returns an RFC 9562 UUIDv7. The timestamp provides useful local
// ordering while the remaining 74 random bits keep identities collision-safe.
func newUUIDv7(now time.Time) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read UUIDv7 entropy: %w", err)
	}

	unixMillis := uint64(now.UTC().UnixMilli())
	raw[0] = byte(unixMillis >> 40)
	raw[1] = byte(unixMillis >> 32)
	raw[2] = byte(unixMillis >> 24)
	raw[3] = byte(unixMillis >> 16)
	raw[4] = byte(unixMillis >> 8)
	raw[5] = byte(unixMillis)
	raw[6] = 0x70 | (raw[6] & 0x0f)
	raw[8] = 0x80 | (raw[8] & 0x3f)

	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

// NewUUIDv7 allocates a canonical UUIDv7 for callers that need to bind an ID
// to filesystem materialization before the corresponding control-plane row is
// inserted. Store repositories validate the same canonical form.
func NewUUIDv7() (string, error) {
	return newUUIDv7(time.Now().UTC())
}

// ValidateUUIDv7 accepts only canonical lowercase UUIDv7 text. It is useful
// at application boundaries where an ID was allocated before Store receives it.
func ValidateUUIDv7(value string) error {
	if !isUUIDv7(value) {
		return ErrInvalidUUIDv7Identity
	}
	return nil
}

func isUUIDv7(value string) bool {
	if value != strings.TrimSpace(value) || value != strings.ToLower(value) {
		return false
	}
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	if value[14] != '7' || value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return false
	}
	for i := 0; i < len(value); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(value[i] >= '0' && value[i] <= '9') && !(value[i] >= 'a' && value[i] <= 'f') {
			return false
		}
	}
	return true
}

func normalizeUUIDv7(value string, now time.Time) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return newUUIDv7(now)
	}
	if !isUUIDv7(value) {
		return "", ErrInvalidUUIDv7Identity
	}
	return value, nil
}
