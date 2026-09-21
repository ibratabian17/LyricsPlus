package providers

import (
	"strings"
	"testing"
	"time"
)

func TestChallengeIssueAndVerify(t *testing.T) {
	issuer := NewIssuer("secret", 10*time.Minute)
	verifier := NewVerifier("secret", 2)

	tok, challenge, err := issuer.Issue(time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if challenge == "" {
		t.Fatal("empty challenge")
	}

	nonce := ""
	for i := 0; ; i++ {
		cand := itoaTest(i)
		if _, err := verifier.Verify(tok, cand); err == nil {
			nonce = cand
			break
		}
	}
	if nonce == "" {
		t.Fatal("failed to find nonce")
	}

	if _, err := verifier.Verify(tok, nonce); err != nil {
		t.Errorf("valid nonce rejected: %v", err)
	}
	if _, err := verifier.Verify(tok, "wrong-nonce"); err == nil {
		t.Errorf("invalid nonce accepted")
	}
}

func TestChallengeIssuedByDifferentSecret(t *testing.T) {
	issuer := NewIssuer("a", time.Minute)
	verifier := NewVerifier("b", 1)
	tok, _, err := issuer.Issue(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		if _, err := verifier.Verify(tok, itoaTest(i)); err == nil {
			t.Fatalf("token signed with different secret verified @%d", i)
		}
	}
}

func TestChallengeExpiry(t *testing.T) {
	issuer := NewIssuer("s", time.Millisecond)
	verifier := NewVerifier("s", 1)
	tok, _, err := issuer.Issue(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 64; i++ {
		if _, err := verifier.Verify(tok, itoaTest(i)); err == nil {
			t.Fatalf("expired token verified @%d", i)
		}
	}
}

func itoaTest(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var b strings.Builder
	for n > 0 {
		b.WriteByte(byte('0' + n%10))
		n /= 10
	}
	rev := []byte(b.String())
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return string(rev)
}
