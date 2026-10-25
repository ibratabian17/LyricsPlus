package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func newTestBreaker() (*Breaker, *time.Time) {
	b := NewBreaker(BreakerConfig{Threshold: 3, BaseCooldown: 10 * time.Second, MaxCooldown: 40 * time.Second, ProbeTimeout: 5 * time.Second})
	now := time.Unix(1_000_000, 0)
	b.now = func() time.Time { return now }
	return b, &now
}

func TestBreakerOpensAfterThresholdAndProbes(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 2; i++ {
		if !b.Allow("apple") {
			t.Fatal("closed breaker must allow")
		}
		b.Report("apple", false)
	}
	if !b.Allow("apple") {
		t.Fatal("below threshold must allow")
	}
	b.Report("apple", false) // 3rd consecutive failure -> open
	if b.Allow("apple") {
		t.Fatal("open breaker must reject")
	}
	if !b.Status("apple").Open {
		t.Fatal("status should be open")
	}

	*now = now.Add(11 * time.Second) // cooldown elapsed -> one probe
	if !b.Allow("apple") {
		t.Fatal("half-open must admit one probe")
	}
	if b.Allow("apple") {
		t.Fatal("only one probe at a time")
	}
	b.Report("apple", false) // failed probe: cooldown doubles to 20s
	*now = now.Add(11 * time.Second)
	if b.Allow("apple") {
		t.Fatal("cooldown should have doubled")
	}
	*now = now.Add(10 * time.Second)
	if !b.Allow("apple") {
		t.Fatal("probe expected after doubled cooldown")
	}
	b.Report("apple", true) // success closes
	if !b.Allow("apple") || b.Status("apple").Open {
		t.Fatal("success must close the breaker")
	}
}

func TestBreakerSuccessResetsConsecutiveCount(t *testing.T) {
	b, _ := newTestBreaker()
	b.Report("qq", false)
	b.Report("qq", false)
	b.Report("qq", true)
	b.Report("qq", false)
	b.Report("qq", false)
	if !b.Allow("qq") {
		t.Fatal("interleaved success must reset the failure streak")
	}
}

func TestBreakerStaleProbeTimesOutAndNeutral(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 3; i++ {
		b.Report("mxm", false)
	}
	*now = now.Add(11 * time.Second)
	if !b.Allow("mxm") {
		t.Fatal("probe expected")
	}
	b.Neutral("mxm") // cancelled probe frees the slot
	if !b.Allow("mxm") {
		t.Fatal("neutral must release the probe slot")
	}
	*now = now.Add(6 * time.Second) // probe never reported: timeout frees it
	if !b.Allow("mxm") {
		t.Fatal("stuck probe must expire")
	}
}

func TestBreakerNil(t *testing.T) {
	var b *Breaker
	if !b.Allow("x") {
		t.Fatal("nil breaker allows")
	}
	b.Report("x", false)
	b.Neutral("x")
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestIsHealthFailure(t *testing.T) {
	yes := []error{
		context.DeadlineExceeded,
		timeoutErr{},
		errors.New("apple music lyrics returned status 429"),
		errors.New("status 503"),
		errors.New("musixmatch web error (401): captcha"),
		errors.New("musixmatch: login failed with status 401: bad"),
		errors.New("spotify: web token in cooldown"),
		errors.New("apple music: no account available"),
		fmt.Errorf("wrap: %w", context.DeadlineExceeded),
	}
	for _, e := range yes {
		if !IsHealthFailure(e) {
			t.Errorf("expected health failure: %v", e)
		}
	}
	no := []error{
		nil,
		context.Canceled,
		errors.New("status 404"),
		errors.New("musixmatch web error (404): not found"),
		errors.New("qq api response missing req"),
		errors.New("no lyrics found"),
	}
	for _, e := range no {
		if IsHealthFailure(e) {
			t.Errorf("expected clean miss: %v", e)
		}
	}
}
