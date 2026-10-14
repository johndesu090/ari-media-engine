// Package id generates short, collision-resistant identifiers used for
// broadcasts, streams, sessions and nodes.
package id

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a random identifier. When prefix is non-empty the result is
// "<prefix>_<hex>"; otherwise the bare hex string is returned.
func New(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail on supported platforms; if it does we
		// cannot safely continue generating identifiers.
		panic("id: crypto/rand failed: " + err.Error())
	}
	encoded := hex.EncodeToString(b)
	if prefix == "" {
		return encoded
	}
	return prefix + "_" + encoded
}
