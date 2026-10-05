package providers

import "sync/atomic"

// maxAccountRetries is the number of account rotations attempted when a
// provider call fails with 401/429 (or 503 for Apple Music).
const maxAccountRetries = 3

// AccountManager holds a fixed list of credentials with deterministic
// round-robin "next" selection used when a provider call fails with 401/429.
type AccountManager[T any] struct {
	accounts     []T
	currentIndex atomic.Int64
}

func newAccountManager[T any](accounts []T) *AccountManager[T] {
	return &AccountManager[T]{accounts: accounts}
}

// Count reports how many accounts are configured.
func (m *AccountManager[T]) Count() int { return len(m.accounts) }

// CurrentIndex returns the active account index.
func (m *AccountManager[T]) CurrentIndex() int {
	if len(m.accounts) == 0 {
		return 0
	}
	cur := m.currentIndex.Load()
	if cur < 0 {
		cur = 0
	}
	return int(cur % int64(len(m.accounts)))
}

// Current returns the currently active account and its index.
func (m *AccountManager[T]) Current() (T, int, bool) {
	if len(m.accounts) == 0 {
		var zero T
		return zero, 0, false
	}
	idx := m.CurrentIndex()
	return m.accounts[idx], idx, true
}

// At returns the account at index i (zero value and false if out of range).
func (m *AccountManager[T]) At(i int) (T, bool) {
	if i < 0 || i >= len(m.accounts) {
		var zero T
		return zero, false
	}
	return m.accounts[i], true
}

func (m *AccountManager[T]) First() (T, bool) { return m.At(0) }

// Rotate advances the active index and returns the new index.
func (m *AccountManager[T]) Rotate() int {
	if len(m.accounts) <= 1 {
		return 0
	}
	return int(m.currentIndex.Add(1) % int64(len(m.accounts)))
}

// Next returns the index following i, wrapping around the list. ok=false when
// there is no further account to rotate to (len <= 1).
func (m *AccountManager[T]) Next(i int) (int, bool) {
	if len(m.accounts) <= 1 {
		return 0, false
	}
	if i < 0 {
		i = 0
	}
	next := (i + 1) % len(m.accounts)
	m.currentIndex.Store(int64(next))
	return next, true
}
