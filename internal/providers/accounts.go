package providers

import "sync/atomic"

const maxAccountRetries = 3

type AccountManager[T any] struct {
	accounts     []T
	currentIndex atomic.Int64
}

func newAccountManager[T any](accounts []T) *AccountManager[T] {
	return &AccountManager[T]{accounts: accounts}
}

func (m *AccountManager[T]) Count() int { return len(m.accounts) }

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

func (m *AccountManager[T]) Current() (T, int, bool) {
	if len(m.accounts) == 0 {
		var zero T
		return zero, 0, false
	}
	idx := m.CurrentIndex()
	return m.accounts[idx], idx, true
}

func (m *AccountManager[T]) At(i int) (T, bool) {
	if i < 0 || i >= len(m.accounts) {
		var zero T
		return zero, false
	}
	return m.accounts[i], true
}

func (m *AccountManager[T]) First() (T, bool) { return m.At(0) }

func (m *AccountManager[T]) Rotate() int {
	if len(m.accounts) <= 1 {
		return 0
	}
	return int(m.currentIndex.Add(1) % int64(len(m.accounts)))
}

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
