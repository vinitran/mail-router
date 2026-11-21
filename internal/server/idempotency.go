package server

import (
	"sync"
	"time"

	"mail-router/internal/email"
)

// Idempotency remembers which idempotency keys were sent, so a caller that
// retries after a timeout or crash (a Kafka redelivery, an outbox retry)
// does not send the same email twice. Only successful sends are
// remembered; a failed send can be retried at once. The memory is per
// process: a restart forgets it (providers with their own idempotency,
// Resend and Brevo, still drop the duplicate).
type Idempotency struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	entries map[string]*idemEntry
}

type idemEntry struct {
	inFlight bool
	result   email.Result
	expires  time.Time
}

// NewIdempotency keeps keys for ttl, at most maxEntries at a time (the
// oldest are dropped first when full).
func NewIdempotency(ttl time.Duration, maxEntries int) *Idempotency {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxEntries <= 0 {
		maxEntries = 100_000
	}
	return &Idempotency{ttl: ttl, maxEntries: maxEntries, now: time.Now, entries: map[string]*idemEntry{}}
}

// Claim reserves key for one send. It returns the cached result when key
// was already sent, ok=false when another request is sending it right now,
// and otherwise a Claim the caller must Release (after Done on success).
func (i *Idempotency) Claim(key string) (claim *Claim, cached *email.Result, ok bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := i.now()
	if e, found := i.entries[key]; found {
		switch {
		case e.inFlight:
			return nil, nil, false
		case now.Before(e.expires):
			result := e.result
			return nil, &result, true
		}
		delete(i.entries, key)
	}
	if len(i.entries) >= i.maxEntries {
		i.evictLocked(now)
	}
	i.entries[key] = &idemEntry{inFlight: true}
	return &Claim{owner: i, key: key}, nil, true
}

// evictLocked drops expired entries and, when still full, the entries that
// expire first.
func (i *Idempotency) evictLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for key, e := range i.entries {
		if e.inFlight {
			continue
		}
		if !now.Before(e.expires) {
			delete(i.entries, key)
			continue
		}
		if oldestKey == "" || e.expires.Before(oldest) {
			oldestKey, oldest = key, e.expires
		}
	}
	if len(i.entries) >= i.maxEntries && oldestKey != "" {
		delete(i.entries, oldestKey)
	}
}

// Len is the number of remembered or in-flight keys.
func (i *Idempotency) Len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.entries)
}

// Claim is one reserved idempotency key.
type Claim struct {
	owner *Idempotency
	key   string
	done  bool
}

// Done records the accepted result for the key's lifetime.
func (c *Claim) Done(result email.Result) {
	c.owner.mu.Lock()
	defer c.owner.mu.Unlock()
	c.owner.entries[c.key] = &idemEntry{result: result, expires: c.owner.now().Add(c.owner.ttl)}
	c.done = true
}

// Release frees the key when the send did not succeed.
func (c *Claim) Release() {
	c.owner.mu.Lock()
	defer c.owner.mu.Unlock()
	if !c.done {
		delete(c.owner.entries, c.key)
	}
}
