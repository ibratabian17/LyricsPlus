package providers

// maxAccountRetries is the number of account rotations attempted when a
// provider call fails with 401/429 (or 503 for Apple Music).
const maxAccountRetries = 3

// AccountManager holds a fixed list of credentials with deterministic
// round-robin "next" selection (starting at index 0) used when a provider
// call fails with 401/429.
type AccountManager[T any] struct {
	accounts []T
}

func newAccountManager[T any](accounts []T) *AccountManager[T] {
	return &AccountManager[T]{accounts: accounts}
}

// Count reports how many accounts are configured.
func (m *AccountManager[T]) Count() int { return len(m.accounts) }

// At returns the account at index i (zero value and false if out of range).
func (m *AccountManager[T]) At(i int) (T, bool) {
	if i < 0 || i >= len(m.accounts) {
		var zero T
		return zero, false
	}
	return m.accounts[i], true
}

func (m *AccountManager[T]) First() (T, bool) { return m.At(0) }

// Next returns the index following i, wrapping around the list. ok=false when
// there is no further account to rotate to (len <= 1).
func (m *AccountManager[T]) Next(i int) (int, bool) {
	if len(m.accounts) <= 1 {
		return 0, false
	}
	if i < 0 {
		i = 0
	}
	return (i + 1) % len(m.accounts), true
}
