package email

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBuildFallbackWiring(t *testing.T) {
	primary := &recordingSender{name: "test-primary", errs: []error{Classify("test-primary", ErrQuotaExceeded, errors.New("daily quota"))}}
	fallback := &recordingSender{name: "test-fallback"}
	registerTestProvider(t, "test-primary", primary, nil)
	registerTestProvider(t, "test-fallback", fallback, nil)
	var logs []string
	d, warnings, err := Build(Settings{Provider: "TEST-PRIMARY", Fallback: "test-fallback", Logf: func(f string, a ...any) { logs = append(logs, f) }})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err %v warnings %v", err, warnings)
	}
	if strings.Join(d.Providers, ",") != "test-primary,test-fallback" || d.Routing != RoutingFailover {
		t.Fatalf("delivery %+v", d)
	}
	result, err := d.Sender.Send(context.Background(), testMessage())
	if err != nil || result.Provider != "test-fallback" || len(primary.sent) != 1 || len(fallback.sent) != 1 {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(logs) != 1 || d.Counters.Snapshot()["test-fallback ok"] != 1 {
		t.Fatalf("logs %v stats %v", logs, d.Counters.Snapshot())
	}
	// A broken fallback is reported but does not disable the primary.
	d, warnings, err = Build(Settings{Provider: "test-primary", Fallback: "not-registered"})
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "fallback disabled") {
		t.Fatalf("err %v warnings %v", err, warnings)
	}
	if _, err := d.Sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRoundRobinOverAProviderList(t *testing.T) {
	a, b := &recordingSender{name: "test-a"}, &recordingSender{name: "test-b"}
	registerTestProvider(t, "test-a", a, nil)
	registerTestProvider(t, "test-b", b, nil)
	registerTestProvider(t, "test-broken", nil, errors.New("TEST_KEY required"))
	d, warnings, err := Build(Settings{Provider: "test-a:2, test-broken, test-b", Routing: "round_robin"})
	if err != nil {
		t.Fatal(err)
	}
	// A provider that cannot be built is reported and left out.
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "provider disabled") || !strings.Contains(warnings[0].Error(), "TEST_KEY required") {
		t.Fatalf("warnings %v", warnings)
	}
	for i := 0; i < 6; i++ {
		if _, err := d.Sender.Send(context.Background(), testMessage()); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.sent) != 4 || len(b.sent) != 2 {
		t.Fatalf("a=%d b=%d", len(a.sent), len(b.sent))
	}
	stats := d.Counters.Snapshot()
	if stats["test-a ok"] != 4 || stats["test-b ok"] != 2 {
		t.Fatalf("stats %v", stats)
	}
}

func TestBuildConfigurationProblems(t *testing.T) {
	registerTestProvider(t, "test-a", &recordingSender{name: "test-a"}, nil)
	if _, _, err := Build(Settings{Provider: "not-registered,also-missing"}); err == nil || !strings.Contains(err.Error(), "also-missing") {
		t.Fatalf("no usable provider: %v", err)
	}
	if _, _, err := Build(Settings{Provider: "test-a:0"}); err == nil || !strings.Contains(err.Error(), "weight") {
		t.Fatalf("bad weight: %v", err)
	}
	d, warnings, err := Build(Settings{Provider: "test-a", Routing: "random"})
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "EMAIL_ROUTING") || d.Routing != RoutingFailover {
		t.Fatalf("unknown routing: %v %v", err, warnings)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(testMessage()); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Message)
		class  error
	}{
		"no recipient":        {func(m *Message) { m.To = nil }, ErrPermanent},
		"bad recipient":       {func(m *Message) { m.To = To("not an address") }, ErrInvalidRecipient},
		"recipient injection": {func(m *Message) { m.To = To("a@example.com\r\nBcc: x@example.com") }, ErrInvalidRecipient},
		"display recipient":   {func(m *Message) { m.To = To("A <a@example.com>") }, ErrInvalidRecipient},
		"no sender":           {func(m *Message) { m.From = Address{} }, ErrPermanent},
		"bad reply-to":        {func(m *Message) { m.ReplyTo = Address{Address: "x"} }, ErrPermanent},
		"no subject":          {func(m *Message) { m.Subject = " " }, ErrPermanent},
		"subject injection":   {func(m *Message) { m.Subject = "a\nBcc: x@example.com" }, ErrPermanent},
		"no body":             {func(m *Message) { m.HTML, m.Text = "", "" }, ErrPermanent},
		"header injection":    {func(m *Message) { m.Headers = map[string]string{"X-A": "b\r\nBcc: x"} }, ErrPermanent},
		"bad header name":     {func(m *Message) { m.Headers = map[string]string{"X A": "b"} }, ErrPermanent},
		"too many recipients": {func(m *Message) {
			m.To = make([]Address, 51)
			for i := range m.To {
				m.To[i] = Address{Address: "a@example.com"}
			}
		}, ErrPermanent},
	} {
		msg := testMessage()
		tc.mutate(&msg)
		if err := Validate(msg); !errors.Is(err, tc.class) {
			t.Errorf("%s: %v, want %s", name, err, ClassName(tc.class))
		}
	}
}
