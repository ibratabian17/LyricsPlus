package providers

import (
	"sync/atomic"
	"time"
)

const maxAccountRetries = 3

type AccountManager[T any] struct {
	accounts     []T
	cooldowns    []atomic.Int64
	currentIndex atomic.Int64
}

func newAccountManager[T any](accounts []T) *AccountManager[T] {
	return &AccountManager[T]{
		accounts:  accounts,
		cooldowns: make([]atomic.Int64, len(accounts)),
	}
}

func (m *AccountManager[T]) Count() int { return len(m.accounts) }

func (m *AccountManager[T]) isCoolingDown(i int, nowNs int64) bool {
	if i < 0 || i >= len(m.cooldowns) {
		return false
	}
	until := m.cooldowns[i].Load()
	return until > 0 && nowNs < until
}

func (m *AccountManager[T]) MarkBad(i int, d time.Duration) {
	if i < 0 || i >= len(m.cooldowns) || d <= 0 {
		return
	}
	m.cooldowns[i].Store(time.Now().Add(d).UnixNano())
}

func (m *AccountManager[T]) CurrentIndex() int {
	n := len(m.accounts)
	if n == 0 {
		return 0
	}
	cur := m.currentIndex.Load()
	if cur < 0 {
		cur = 0
	}
	idx := int(cur % int64(n))
	if n == 1 {
		return idx
	}
	nowNs := time.Now().UnixNano()
	if !m.isCoolingDown(idx, nowNs) {
		return idx
	}
	for step := 1; step < n; step++ {
		cand := (idx + step) % n
		if !m.isCoolingDown(cand, nowNs) {
			m.currentIndex.Store(int64(cand))
			return cand
		}
	}
	return idx
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
	next, _ := m.Next(int(m.currentIndex.Load()))
	return next
}

func (m *AccountManager[T]) Next(i int) (int, bool) {
	n := len(m.accounts)
	if n <= 1 {
		return 0, false
	}
	if i < 0 {
		i = 0
	}
	nowNs := time.Now().UnixNano()
	for step := 1; step < n; step++ {
		cand := (i + step) % n
		if !m.isCoolingDown(cand, nowNs) {
			m.currentIndex.Store(int64(cand))
			return cand, true
		}
	}
	next := (i + 1) % n
	m.currentIndex.Store(int64(next))
	return next, true
}
