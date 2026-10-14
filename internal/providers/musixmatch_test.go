package providers

import (
	"strings"
	"testing"
	"time"
)

func TestSignature(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	sig := Signature("track.richsync.get", now)
	if len(sig) == 0 {
		t.Fatal("empty signature")
	}
	if strings.ContainsAny(sig, "/+=?") {
		t.Errorf("signature must be base64url-safe, got %q", sig)
	}
	if Signature("track.richsync.get", now.Add(24*time.Hour)) == sig {
		t.Errorf("signature should change daily")
	}
	if Signature("track.subtitle.get", now) == sig {
		t.Errorf("signature should include the endpoint")
	}
}

func TestSignedURL(t *testing.T) {
	u := SignedURL("https://apic.musixmatch.com/ws/1.1/track.richsync.get?track_id=1")
	if !strings.Contains(u, "signature=") || !strings.Contains(u, "signature_protocol=sha1") {
		t.Errorf("signed url missing params: %q", u)
	}
}
