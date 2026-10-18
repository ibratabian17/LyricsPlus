package lyricsplus

import (
	"time"

	"lyricsplus/backend/internal/providers"
	"lyricsplus/backend/internal/proxy"
)

func NewIssuer(secret string, ttl time.Duration) *providers.Issuer {
	return providers.NewIssuer(secret, ttl)
}

func NewVerifier(secret string, difficulty int) *providers.Verifier {
	return providers.NewVerifier(secret, difficulty)
}

func NewLyricsPlus(client *proxy.Client) *providers.LyricsPlusProvider {
	return providers.NewLyricsPlus(client)
}

type (
	Issuer   = providers.Issuer
	Verifier = providers.Verifier
	Provider = providers.LyricsPlusProvider
)
