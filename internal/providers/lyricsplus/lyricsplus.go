package lyricsplus

import (
	"time"

	"lyricsplus/backend/internal/providers"
)

func NewIssuer(secret string, ttl time.Duration) *providers.Issuer {
	return providers.NewIssuer(secret, ttl)
}

func NewVerifier(secret string, difficulty int) *providers.Verifier {
	return providers.NewVerifier(secret, difficulty)
}

type (
	Issuer   = providers.Issuer
	Verifier = providers.Verifier
)
