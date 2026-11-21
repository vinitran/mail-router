package email

import (
	"context"
	"sync"
	"time"
)

// RateLimit spaces sends so that at most perSecond messages start each
// second (EMAIL_RATE_LIMIT_PER_SECOND). perSecond <= 0 returns next
// unchanged (the default). Waiting honours ctx.
func RateLimit(next Sender, perSecond float64) Sender {
	if perSecond <= 0 {
		return next
	}
	return &rateLimitedSender{next: next, interval: time.Duration(float64(time.Second) / perSecond), now: time.Now, sleep: sleepContext}
}

type rateLimitedSender struct {
	next     Sender
	interval time.Duration
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error

	mu        sync.Mutex
	nextStart time.Time // earliest start of the next send
}

func (r *rateLimitedSender) Send(ctx context.Context, msg Message) (Result, error) {
	r.mu.Lock()
	now := r.now()
	start := r.nextStart
	if start.Before(now) {
		start = now
	}
	r.nextStart = start.Add(r.interval)
	r.mu.Unlock()
	if wait := start.Sub(now); wait > 0 {
		if err := r.sleep(ctx, wait); err != nil {
			return Result{}, Classify("", ErrTemporary, err)
		}
	}
	return r.next.Send(ctx, msg)
}
