package storage

import (
	"context"
	"sync"
	"time"
)

const submitRetryInterval = 2 * time.Millisecond

type writeJob struct {
	ctx  context.Context
	run  func(context.Context)
	done func(error)
}

type writeQueue struct {
	jobs chan *writeJob
	wg   sync.WaitGroup
	once sync.Once

	closeMu sync.RWMutex
	closed  bool

	mu       sync.Mutex
	shed     int64
	inflight int
}

func newWriteQueue(workers, capacity int) *writeQueue {
	if workers <= 0 {
		workers = 1
	}
	if capacity <= 0 {
		capacity = 1024
	}
	q := &writeQueue{
		jobs: make(chan *writeJob, capacity),
	}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
	return q
}

func (q *writeQueue) worker() {
	defer q.wg.Done()
	for job := range q.jobs {
		q.execute(job)
	}
}

func (q *writeQueue) execute(job *writeJob) {
	defer func() {
		if job.done != nil {
			job.done(nil)
		}
	}()
	defer func() {
		if rec := recover(); rec != nil && job.done != nil {
			job.done(errWritePanic)
		}
	}()
	q.mu.Lock()
	q.inflight++
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.inflight--
		q.mu.Unlock()
	}()
	job.run(job.ctx)
}

func (q *writeQueue) Submit(ctx context.Context, run func(context.Context)) {
	if q == nil {
		run(ctx)
		return
	}
	job := &writeJob{ctx: ctx, run: run}

	q.closeMu.RLock()
	defer q.closeMu.RUnlock()
	if q.closed {
		return
	}

	for {
		select {
		case q.jobs <- job:
			return
		default:
		}

		select {
		case victim := <-q.jobs:
			q.mu.Lock()
			q.shed++
			q.mu.Unlock()
			if victim != nil && victim.done != nil {
				victim.done(errQueueFull)
			}
		default:
			select {
			case <-ctx.Done():
				return
			case <-time.After(submitRetryInterval):
			}
		}
	}
}

func (q *writeQueue) Close() {
	if q == nil {
		return
	}
	q.once.Do(func() {
		q.closeMu.Lock()
		q.closed = true
		close(q.jobs)
		q.closeMu.Unlock()
		q.wg.Wait()
	})
}

func (q *writeQueue) CloseWithin(d time.Duration) {
	if q == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		q.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

func (q *writeQueue) Stats() (shed int64, inflight, queued int) {
	if q == nil {
		return 0, 0, 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.shed, q.inflight, len(q.jobs)
}
