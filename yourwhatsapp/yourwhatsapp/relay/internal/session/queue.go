package session

import (
	"math/rand/v2"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// sendJob is one accepted message waiting in an account's queue. The
// attachment bytes live only here, in memory.
type sendJob struct {
	localID     string
	to          types.JID
	contentType string
	text        string
	media       []byte
	mimetype    string
	filename    string
}

// sendQueue is an account's FIFO of accepted sends with a single worker that
// spaces sends by a random anti-ban delay.
type sendQueue struct {
	capacity           int
	delayMin, delayMax time.Duration
	sleep              func(time.Duration)
	rnd                func() float64

	mu       sync.Mutex
	jobs     []*sendJob
	stopped  bool
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	lastSend time.Time // worker goroutine only
}

// newSendQueue builds a queue. A nil sleep waits on a real timer that returns
// early on shutdown; a nil rnd uses math/rand.
func newSendQueue(capacity int, delayMin, delayMax time.Duration, sleep func(time.Duration), rnd func() float64) *sendQueue {
	q := &sendQueue{
		capacity: capacity, delayMin: delayMin, delayMax: delayMax, sleep: sleep, rnd: rnd,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	if q.sleep == nil {
		q.sleep = q.interruptibleSleep
	}
	if q.rnd == nil {
		q.rnd = rand.Float64
	}
	return q
}

// start runs the worker; deliver performs one send.
func (q *sendQueue) start(deliver func(*sendJob)) {
	go q.run(deliver)
}

// full reports whether the queue already holds capacity waiting jobs.
func (q *sendQueue) full() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs) >= q.capacity
}

// push appends a job; false means the queue is full or stopped.
func (q *sendQueue) push(job *sendJob) bool {
	q.mu.Lock()
	if q.stopped || len(q.jobs) >= q.capacity {
		q.mu.Unlock()
		return false
	}
	q.jobs = append(q.jobs, job)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// shutdown stops the worker, waits up to wait for an in-flight send, and
// returns the jobs that never started.
func (q *sendQueue) shutdown(wait time.Duration) []*sendJob {
	q.mu.Lock()
	if !q.stopped {
		q.stopped = true
		close(q.stop)
	}
	q.mu.Unlock()
	select {
	case <-q.done:
	case <-time.After(wait):
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	left := q.jobs
	q.jobs = nil
	return left
}

func (q *sendQueue) nextDelay() time.Duration {
	if q.delayMax <= q.delayMin {
		return q.delayMin
	}
	return q.delayMin + time.Duration(q.rnd()*float64(q.delayMax-q.delayMin))
}

func (q *sendQueue) run(deliver func(*sendJob)) {
	defer close(q.done)
	for {
		job := q.pop()
		if job == nil {
			return
		}
		if !q.lastSend.IsZero() {
			if wait := q.nextDelay() - time.Since(q.lastSend); wait > 0 {
				q.sleep(wait)
			}
		}
		if q.isStopped() {
			q.requeueFront(job)
			return
		}
		deliver(job)
		q.lastSend = time.Now()
	}
}

// pop blocks until a job is available; nil means the queue was stopped.
func (q *sendQueue) pop() *sendJob {
	for {
		q.mu.Lock()
		if q.stopped {
			q.mu.Unlock()
			return nil
		}
		if len(q.jobs) > 0 {
			job := q.jobs[0]
			q.jobs = q.jobs[1:]
			q.mu.Unlock()
			return job
		}
		q.mu.Unlock()
		select {
		case <-q.wake:
		case <-q.stop:
		}
	}
}

func (q *sendQueue) requeueFront(job *sendJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append([]*sendJob{job}, q.jobs...)
}

func (q *sendQueue) isStopped() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stopped
}

func (q *sendQueue) interruptibleSleep(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-q.stop:
	}
}
