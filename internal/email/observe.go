package email

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Event describes one send through Instrument.
type Event struct {
	Provider string // provider that accepted the message, when known
	Category string
	Duration time.Duration
	Err      error
}

// Observer receives an Event per send (logging, metrics, ...).
type Observer interface {
	Observe(Event)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(Event)

func (f ObserverFunc) Observe(e Event) { f(e) }

// Instrument reports every send to the observers. It never changes the
// result.
func Instrument(next Sender, observers ...Observer) Sender {
	if len(observers) == 0 {
		return next
	}
	return &instrumentedSender{next: next, observers: observers, now: time.Now}
}

type instrumentedSender struct {
	next      Sender
	observers []Observer
	now       func() time.Time
}

func (s *instrumentedSender) Send(ctx context.Context, msg Message) (Result, error) {
	start := s.now()
	result, err := s.next.Send(ctx, msg)
	provider := result.Provider
	var classified *Error
	if provider == "" && errors.As(err, &classified) {
		provider = classified.Provider
	}
	event := Event{Provider: provider, Category: msg.Category, Duration: s.now().Sub(start), Err: err}
	for _, o := range s.observers {
		o.Observe(event)
	}
	return result, err
}

// LogFailures logs failed sends with their class only: provider error text
// can contain the recipient address, so it is not logged here (callers keep
// their own logging as before).
func LogFailures(logf func(format string, args ...any)) Observer {
	return ObserverFunc(func(e Event) {
		if e.Err != nil {
			logf("email: send %q failed: %s after %s", e.Category, ClassName(e.Err), e.Duration.Round(time.Millisecond))
		}
	})
}

// Counters counts sends by provider and outcome ("resend ok",
// "ses rate_limited", ...). GET /v1/stats exposes a snapshot.
type Counters struct {
	mu     sync.Mutex
	counts map[string]uint64
}

func (c *Counters) Observe(e Event) {
	provider := e.Provider
	if provider == "" {
		provider = "unknown"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]uint64{}
	}
	c.counts[provider+" "+ClassName(e.Err)]++
}

// Snapshot returns a copy of the counts.
func (c *Counters) Snapshot() map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]uint64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}
