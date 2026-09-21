package providers

import (
	"strconv"
	"strings"
	"testing"
)

func TestDeriveSecretBytes(t *testing.T) {
	cipher := BestSecret()
	secret := DeriveSecretBytes(cipher)
	if len(secret) == 0 {
		t.Fatal("empty derived secret")
	}
	if !strings.EqualFold(string(secret), string(DeriveSecretBytes(cipher))) {
		t.Error("derivation must be deterministic")
	}
	// Spot check first byte: e[0] ^ (0 + 9) -> stringified.
	want := []byte(itoa(int(cipher[0] ^ 9)))
	if secret[0] != want[0] {
		t.Errorf("first derived byte = %d, want %d", secret[0], want[0])
	}
}

func TestGenerateTOTPFormat(t *testing.T) {
	secret := DeriveSecretBytes(BestSecret())
	totp := GenerateTOTP(secret, 1700000000, 6, 30)
	if len(totp) != 6 {
		t.Errorf("totp length = %d", len(totp))
	}
	for _, c := range totp {
		if c < '0' || c > '9' {
			t.Errorf("totp has non-digit %c", c)
		}
	}
	totp2 := GenerateTOTP(secret, 1700000000, 6, 30)
	if totp != totp2 {
		t.Errorf("totp must be deterministic")
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
