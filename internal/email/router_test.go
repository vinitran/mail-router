package email

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRouterFailsOverOnEverythingButAnInvalidRecipient(t *testing.T) {
	for _, tc := range []struct {
		err          error
		wantFallback bool
	}{
		{Classify("resend", ErrRateLimited, errors.New("429")), true},
		{Classify("resend", ErrQuotaExceeded, errors.New("daily quota")), true},
		{fmt.Errorf("send: %w", Classify("smtp", ErrTemporary, errors.New("connect"))), true},
		{Classify("brevo", ErrPermanent, errors.New("401 unrecognised IP address")), true},
		{errors.New("unclassified"), true},
		{Classify("resend", ErrInvalidRecipient, errors.New("bad to")), false},
	} {
		primary := &recordingSender{name: "primary", errs: []error{tc.err}}
		fallback := &recordingSender{name: "fallback"}
		sender := Router([]Route{{Name: "primary", Sender: primary}}, []Route{{Name: "fallback", Sender: fallback}}, RouterOptions{})
		result, err := sender.Send(context.Background(), testMessage())
		if tc.wantFallback != (len(fallback.sent) == 1) {
			t.Errorf("%v: fallback used = %v", tc.err, len(fallback.sent) == 1)
		}
		if tc.wantFallback && (err != nil || result.Provider != "fallback") {
			t.Errorf("%v: result %+v err %v", tc.err, result, err)
		}
		if !tc.wantFallback && err != tc.err {
			t.Errorf("%v: primary error must pass through, got %v", tc.err, err)
		}
	}
}

func TestRouterReportsEveryProviderTried(t *testing.T) {
	sender := Router([]Route{
		{Name: "a", Sender: &recordingSender{errs: []error{Classify("a", ErrRateLimited, errors.New("429"))}}},
		{Name: "b", Sender: &recordingSender{errs: []error{Classify("b", ErrTemporary, errors.New("503"))}}},
	}, []Route{
		{Name: "c", Sender: &recordingSender{errs: []error{Classify("c", ErrInvalidRecipient, errors.New("no such user"))}}},
		{Name: "d", Sender: &recordingSender{}},
	}, RouterOptions{})
	_, err := sender.Send(context.Background(), testMessage())
	if !errors.Is(err, ErrInvalidRecipient) || !strings.Contains(err.Error(), "providers tried: a rate_limited, b temporary, c invalid_recipient") {
		t.Fatalf("err = %v", err)
	}
}

func TestRouterWithOneRouteDoesNotWrap(t *testing.T) {
	only := &recordingSender{}
	if Router([]Route{{Name: "only", Sender: only}}, nil, RouterOptions{}) != Sender(only) {
		t.Fatal("a single route must not wrap")
	}
}

func TestRouterFailoverAlwaysStartsWithTheFirstProvider(t *testing.T) {
	a, b := &recordingSender{name: "a"}, &recordingSender{name: "b"}
	sender := Router([]Route{{Name: "a", Sender: a, Weight: 1}, {Name: "b", Sender: b, Weight: 5}}, nil, RouterOptions{Routing: RoutingFailover})
	for i := 0; i < 6; i++ {
		if _, err := sender.Send(context.Background(), testMessage()); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.sent) != 6 || len(b.sent) != 0 {
		t.Fatalf("a=%d b=%d", len(a.sent), len(b.sent))
	}
}

func TestRouterRoundRobinFollowsWeightsSmoothly(t *testing.T) {
	a, b, c := &recordingSender{name: "a"}, &recordingSender{name: "b"}, &recordingSender{name: "c"}
	fallback := &recordingSender{name: "fallback"}
	sender := Router(
		[]Route{{Name: "a", Sender: a, Weight: 3}, {Name: "b", Sender: b, Weight: 1}, {Name: "c", Sender: c}},
		[]Route{{Name: "fallback", Sender: fallback, Weight: 100}},
		RouterOptions{Routing: RoutingRoundRobin})
	var order []string
	for i := 0; i < 10; i++ {
		result, err := sender.Send(context.Background(), testMessage())
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, result.Provider)
	}
	if got := strings.Join(order, " "); got != "a b a c a a b a c a" {
		t.Fatalf("order %q", got)
	}
	if len(fallback.sent) != 0 {
		t.Fatal("a fallback provider must never be in the rotation")
	}
}

func TestRouterRoundRobinMovesOnAndCoolsDownAProviderOverQuota(t *testing.T) {
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	a := &recordingSender{name: "a", errs: []error{Classify("a", ErrQuotaExceeded, errors.New("daily quota"))}}
	b := &recordingSender{name: "b"}
	var logs []string
	sender := Router([]Route{{Name: "a", Sender: a}, {Name: "b", Sender: b}}, nil, RouterOptions{
		Routing:  RoutingRoundRobin,
		Cooldown: time.Hour,
		Logf:     func(f string, args ...any) { logs = append(logs, f) },
		now:      func() time.Time { return now },
	})
	// a is picked first, fails over quota, b takes the message.
	result, err := sender.Send(context.Background(), testMessage())
	if err != nil || result.Provider != "b" || len(a.sent) != 1 {
		t.Fatalf("result %+v err %v a=%d", result, err, len(a.sent))
	}
	if len(logs) != 1 || strings.Contains(strings.Join(logs, ""), "user@example.com") {
		t.Fatalf("logs %q", logs)
	}
	// During the cooldown a is not contacted at all.
	for i := 0; i < 4; i++ {
		if result, err := sender.Send(context.Background(), testMessage()); err != nil || result.Provider != "b" {
			t.Fatalf("cooling: %+v %v", result, err)
		}
	}
	if len(a.sent) != 1 {
		t.Fatalf("a contacted %d times during its cooldown", len(a.sent))
	}
	// After the cooldown a is back in the rotation.
	now = now.Add(time.Hour + time.Second)
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		result, err := sender.Send(context.Background(), testMessage())
		if err != nil {
			t.Fatal(err)
		}
		seen[result.Provider]++
	}
	if seen["a"] != 2 || seen["b"] != 2 {
		t.Fatalf("after cooldown %v", seen)
	}
}

func TestRouterTriesCoolingProvidersWhenNothingElseIsLeft(t *testing.T) {
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	quota := func(name string) error { return Classify(name, ErrQuotaExceeded, errors.New("quota")) }
	a := &recordingSender{name: "a", errs: []error{quota("a")}}
	b := &recordingSender{name: "b", errs: []error{quota("b")}}
	sender := Router([]Route{{Name: "a", Sender: a}, {Name: "b", Sender: b}}, nil, RouterOptions{now: func() time.Time { return now }})
	if _, err := sender.Send(context.Background(), testMessage()); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v", err)
	}
	// Both are cooling down; they are still tried in order rather than
	// failing the message without a request.
	result, err := sender.Send(context.Background(), testMessage())
	if err != nil || result.Provider != "a" {
		t.Fatalf("result %+v err %v", result, err)
	}
}

func TestRouterStopsWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &recordingSender{name: "a", errs: []error{Classify("a", ErrTemporary, errors.New("timeout"))}}
	b := &recordingSender{name: "b"}
	sender := Router([]Route{{Name: "a", Sender: SenderFunc(func(ctx context.Context, msg Message) (Result, error) {
		cancel()
		return a.Send(ctx, msg)
	})}, {Name: "b", Sender: b}}, nil, RouterOptions{})
	if _, err := sender.Send(ctx, testMessage()); err == nil || len(b.sent) != 0 {
		t.Fatalf("err %v, b=%d", err, len(b.sent))
	}
}

func TestParseProviders(t *testing.T) {
	got, err := ParseProviders(" Brevo:3, sendgrid ,, resend:1, brevo:9 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []ProviderSpec{{"brevo", 3}, {"sendgrid", 1}, {"resend", 1}}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v, want %v", got, want)
		}
	}
	for _, bad := range []string{"brevo:0", "brevo:x", ":2", "brevo:2000"} {
		if _, err := ParseProviders(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got, err := ParseProviders(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}
