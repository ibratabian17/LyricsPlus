package providers

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSign(t *testing.T) {
	sig := Sign(`{"songmid":123}`)
	// part1 = indices [23,14,6,36,16,7,19] (40 filtered out) => 7 chars; part2 => 8 chars.
	// len = 3 + 7 + b64(20 bytes => 27 chars minus /+=) + 8 => 44 or 45.
	if !strings.HasPrefix(sig, "zzc") {
		t.Errorf("signature must start with zzc: %q", sig)
	}
	if strings.Contains(sig, "/") || strings.Contains(sig, "+") || strings.Contains(sig, "=") {
		t.Errorf("signature contains rejected chars: %q", sig)
	}
	if strings.ToLower(sig) != sig {
		t.Errorf("signature must be wholly lowercase: %q", sig)
	}
	if len(sig) != 44 && len(sig) != 45 {
		t.Errorf("signature length = %d, want 44..45 (%q)", len(sig), sig)
	}
}

func TestSignStructure(t *testing.T) {
	payload := `{"songmid":9}`
	sum := sha1.Sum([]byte(payload))
	hashHex := hex.EncodeToString(sum[:])

	part1Idx := []int{23, 14, 6, 36, 16, 40, 7, 19}
	var part1 strings.Builder
	for _, i := range part1Idx {
		if i < 40 {
			part1.WriteByte(hashHex[i])
		}
	}
	if got := Sign(payload); !strings.HasPrefix(got, "zzc"+part1.String()) {
		t.Errorf("part1 mismatch: want prefix zzc%s, got %q", part1.String(), got)
	}
}

func TestSignDeterministic(t *testing.T) {
	x, y := Sign("abc"), Sign("abc")
	if x != y {
		t.Errorf("signature must be deterministic: %q != %q", x, y)
	}
	if Sign("abc") == Sign("abd") {
		t.Errorf("signature must differ for different payloads")
	}
}
