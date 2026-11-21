package email

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRetryRetriesOnlyRetryableErrors(t *testing.T) {
	temporary := Classify("p", ErrTemporary, errors.New("timeout"))
	sender := &recordingSender{errs: []error{temporary, temporary}}
	r := Retry(sender, 3, time.Millisecond).(*retrySender)
	var waits []time.Duration
	r.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	if _, err := r.Send(context.Background(), testMessage()); err != nil || len(sender.sent) != 3 {
		t.Fatalf("err %v attempts %d", err, len(sender.sent))
	}
	if len(waits) != 2 || waits[1] != 2*waits[0] {
		t.Fatalf("backoff %v", waits)
	}

	permanent := &recordingSender{errs: []error{Classify("p", ErrPermanent, errors.New("bad"))}}
	if _, err := Retry(permanent, 3, time.Millisecond).Send(context.Background(), testMessage()); err == nil || len(permanent.sent) != 1 {
		t.Fatal("permanent errors must not be retried")
	}
	exhausted := &recordingSender{errs: []error{temporary, temporary, temporary}}
	if _, err := Retry(exhausted, 2, time.Millisecond).Send(context.Background(), testMessage()); !errors.Is(err, ErrTemporary) || len(exhausted.sent) != 2 {
		t.Fatalf("attempt limit: %v %d", err, len(exhausted.sent))
	}
	if Retry(sender, 1, 0) != Sender(sender) {
		t.Fatal("attempts <= 1 must not wrap")
	}
}

func TestRateLimitSpacesSends(t *testing.T) {
	now := time.Unix(0, 0)
	r := RateLimit(&recordingSender{}, 2).(*rateLimitedSender)
	var waits []time.Duration
	r.now = func() time.Time { return now }
	r.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	for i := 0; i < 3; i++ {
		_, _ = r.Send(context.Background(), testMessage())
	}
	if len(waits) != 2 || waits[0] != 500*time.Millisecond || waits[1] != time.Second {
		t.Fatalf("waits %v", waits)
	}
	r.sleep = func(ctx context.Context, _ time.Duration) error { return context.Canceled }
	if _, err := r.Send(context.Background(), testMessage()); !errors.Is(err, ErrTemporary) {
		t.Fatalf("cancelled wait: %v", err)
	}
	inner := &recordingSender{}
	if RateLimit(inner, 0) != Sender(inner) {
		t.Fatal("0 must not wrap")
	}
}

func TestInstrumentCountsAndLogsWithoutRecipients(t *testing.T) {
	counters := &Counters{}
	var logged []string
	failing := &recordingSender{name: "smtp", errs: []error{Classify("smtp", ErrInvalidRecipient, errors.New("550 user@example.com unknown"))}}
	s := Instrument(failing, LogFailures(func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }), counters)
	_, _ = s.Send(context.Background(), testMessage())
	_, _ = s.Send(context.Background(), testMessage())
	if got := counters.Snapshot(); got["smtp invalid_recipient"] != 1 || got["smtp ok"] != 1 {
		t.Fatalf("counts %v", got)
	}
	if len(logged) != 1 || strings.Contains(logged[0], "user@example.com") || !strings.Contains(logged[0], "invalid_recipient") {
		t.Fatalf("log %v", logged)
	}
}

func TestErrorClasses(t *testing.T) {
	inner := errors.New("failed to set recipient x: 550")
	err := fmt.Errorf("wrapped: %w", Classify("smtp", ErrInvalidRecipient, inner))
	if err.Error() != "wrapped: "+inner.Error() || !errors.Is(err, ErrInvalidRecipient) || !errors.Is(err, inner) || errors.Is(err, ErrTemporary) {
		t.Fatalf("classification through wrapping: %v", err)
	}
	if Classify("x", ErrPermanent, err) != err || Classify("x", ErrPermanent, nil) != nil {
		t.Fatal("an existing class must be kept")
	}
	if !IsRetryable(Classify("", ErrRateLimited, inner)) || IsRetryable(Classify("", ErrQuotaExceeded, inner)) {
		t.Fatal("retryable")
	}
	if ClassOf(context.DeadlineExceeded) != ErrTemporary || ClassName(nil) != "ok" || ClassName(inner) != "unclassified" {
		t.Fatal("class helpers")
	}
}

func TestEnvHelpers(t *testing.T) {
	e := Env(func(k string) string {
		return map[string]string{"N": " 5 ", "B": "yes", "D": "2s", "BAD": "x"}[k]
	})
	if v, err := e.Int("N", 1); v != 5 || err != nil {
		t.Fatal("int")
	}
	if _, err := e.Int("BAD", 1); err == nil {
		t.Fatal("bad int")
	}
	if _, err := e.Bool("B", false); err == nil {
		t.Fatal("bad bool")
	}
	if v, _ := e.Duration("D", time.Second); v != 2*time.Second {
		t.Fatal("duration")
	}
	if Env(nil).String("X", "def") != "def" {
		t.Fatal("nil env")
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	registerTestProvider(t, "test-dup", &recordingSender{}, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic")
		}
	}()
	Register("test-dup", func(Env) (Sender, error) { return nil, nil })
}
