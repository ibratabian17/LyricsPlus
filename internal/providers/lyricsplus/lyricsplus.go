// Package lyricsplus re-exports the proof-of-work challenge/verify machinery
// so the API layer can depend on a stable sub-package name while the
// implementations live in internal/providers.
package lyricsplus

import (
	"time"

	"lyricsplus/backend/internal/providers"
)

// NewIssuer constructs a PoW challenge JWT issuer.
func NewIssuer(secret string, ttl time.Duration) *providers.Issuer {
	return providers.NewIssuer(secret, ttl)
}

// NewVerifier constructs a PoW nonce verifier.
func NewVerifier(secret string, difficulty int) *providers.Verifier {
	return providers.NewVerifier(secret, difficulty)
}

// Issuer and Verifier are aliases so API handlers can reference the types
// through this package without importing internal/providers directly.
type (
	Issuer   = providers.Issuer
	Verifier = providers.Verifier
)
