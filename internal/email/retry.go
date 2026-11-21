package email

import (
	"context"
	"time"
)

// Retry resends on a retryable error (ErrRateLimited, ErrTemporary) up to
// attempts times in total, doubling delay between attempts. attempts <= 1
// returns next unchanged, which is the default: the Kafka consumers and the
// outbox already retry, so in-process retries are opt-in
// (EMAIL_RETRY_ATTEMPTS).
func Retry(next Sender, attempts int, delay time.Duration) Sender {
	if attempts <= 1 {
		return next
	}
	if delay <= 0 {
		delay = 500 * time.Millisecond
	}
	return &retrySender{next: next, attempts: attempts, delay: delay, sleep: sleepContext}
}

type retrySender struct {
	next     Sender
	attempts int
	delay    time.Duration
	sleep    func(context.Context, time.Duration) error
}

func (r *retrySender) Send(ctx context.Context, msg Message) (Result, error) {
	delay := r.delay
	var (
		result Result
		err    error
	)
	for attempt := 1; ; attempt++ {
		result, err = r.next.Send(ctx, msg)
		if err == nil || attempt >= r.attempts || !IsRetryable(err) || ctx.Err() != nil {
			return result, err
		}
		if sleepErr := r.sleep(ctx, delay); sleepErr != nil {
			return result, err
		}
		delay *= 2
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
