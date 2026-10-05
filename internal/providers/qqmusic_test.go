package providers

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSign(t *testing.T) {
	sig := Sign(`{"songmid":123}`)

	if !strings.HasPrefix(sig, "zzc") {
		t.Errorf("signature must start with zzc: %q", sig)
	}
	if strings.Contains(sig, "/") || strings.Contains(sig, "+") || strings.Contains(sig, "=") {
		t.Errorf("signature contains rejected chars: %q", sig)
	}
	if strings.ToLower(sig) != sig {
		t.Errorf("signature must be wholly lowercase: %q", sig)
	}
	if len(sig) < 40 || len(sig) > 46 {
		t.Errorf("signature length = %d, want 40..46 (%q)", len(sig), sig)
	}
}

func TestSignStructure(t *testing.T) {
	payload := `{"songmid":9}`
	sum := sha1.Sum([]byte(payload))
	hashHex := strings.ToUpper(hex.EncodeToString(sum[:]))

	part1Idx := []int{23, 14, 6, 36, 16, 7, 19}
	var part1 strings.Builder
	for _, i := range part1Idx {
		part1.WriteByte(hashHex[i])
	}
	if got := Sign(payload); !strings.HasPrefix(got, strings.ToLower("zzc"+part1.String())) {
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

func TestIsQQMid(t *testing.T) {
	if !isQQMid("002vPwdB2H0fGA") {
		t.Errorf("expected 002vPwdB2H0fGA to be valid QQ mid")
	}
	if isQQMid("1658571287") {
		t.Errorf("Apple Music id 1658571287 should not be QQ mid")
	}
	if isQQMid("4cOdK2wGLETKBW3PvgPWqT") {
		t.Errorf("Spotify id should not be QQ mid")
	}
	if isQQMid("") {
		t.Errorf("empty string should not be QQ mid")
	}
}

func TestProcessLyric(t *testing.T) {

	res, err := processLyric("")
	if err != nil || res != "" {
		t.Errorf("expected empty result, got %q (err=%v)", res, err)
	}

	plainLRC := "[00:01.00]hello world"
	res, err = processLyric(plainLRC)
	if err != nil || !strings.Contains(res, "<QrcInfos>") {
		t.Errorf("expected wrapped XML, got %q (err=%v)", res, err)
	}

	wrappedXML := "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<QrcInfos><LyricInfo></LyricInfo></QrcInfos>"
	res, err = processLyric(wrappedXML)
	if err != nil || res != wrappedXML {
		t.Errorf("expected original XML, got %q", res)
	}
}
