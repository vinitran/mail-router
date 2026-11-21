package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-router/internal/email"
)

const testToken = "0123456789abcdef0123456789abcdef"

type fakeSender struct {
	mu      sync.Mutex
	sent    []email.Message
	errs    []error
	block   chan struct{} // when set, Send waits on it
	ctxErrs []error       // ctx.Err() when Send ran
}

func (f *fakeSender) Send(ctx context.Context, msg email.Message) (email.Result, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return email.Result{Provider: "brevo"}, err
		}
	}
	return email.Result{Provider: "brevo", ProviderMessageID: "m-1"}, nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func newTestServer(sender email.Sender) *Server {
	counters := &email.Counters{}
	return New(Config{
		Token:       testToken,
		DefaultFrom: email.Address{Name: "Hoàn Xu", Address: "noreply@hoanxu.com"},
		Delivery:    &email.Delivery{Sender: email.Instrument(sender, counters), Counters: counters, Providers: []string{"brevo"}, Routing: "failover"},
		Idempotency: NewIdempotency(time.Hour, 10),
	})
}

const validBody = `{"to":[{"address":"user@example.com","name":"Bạn"}],"subject":"Chào","html":"<p>Xin chào</p>","text":"Xin chào","category":"welcome","headers":{"List-Unsubscribe":"<https://hoanxu.com/u>"},"tags":{"server":"hoanxu.com"}}`

func do(t *testing.T, h http.Handler, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func errClass(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["class"].(string)
	return s
}

func TestSendDeliversWithDefaultSender(t *testing.T) {
	sender := &fakeSender{}
	code, out := do(t, newTestServer(sender), "POST", "/v1/send", testToken, validBody)
	if code != 200 || out["provider"] != "brevo" || out["provider_message_id"] != "m-1" {
		t.Fatalf("%d %v", code, out)
	}
	msg := sender.sent[0]
	if msg.From != (email.Address{Name: "Hoàn Xu", Address: "noreply@hoanxu.com"}) || msg.To[0] != (email.Address{Name: "Bạn", Address: "user@example.com"}) {
		t.Fatalf("addresses %+v", msg)
	}
	if msg.Subject != "Chào" || msg.Text != "Xin chào" || msg.Category != "welcome" || msg.Headers["List-Unsubscribe"] == "" || msg.Tags["server"] != "hoanxu.com" {
		t.Fatalf("message %+v", msg)
	}
}

func TestSendKeepsTheCallersSender(t *testing.T) {
	sender := &fakeSender{}
	body := strings.Replace(validBody, `{"to"`, `{"from":{"name":"Hoàn Tiền","address":"noreply@hoantien.work"},"to"`, 1)
	if code, out := do(t, newTestServer(sender), "POST", "/v1/send", testToken, body); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if sender.sent[0].From != (email.Address{Name: "Hoàn Tiền", Address: "noreply@hoantien.work"}) {
		t.Fatalf("from %+v", sender.sent[0].From)
	}
}

func TestAuth(t *testing.T) {
	sender := &fakeSender{}
	s := newTestServer(sender)
	for _, token := range []string{"", "wrong", testToken + "x"} {
		if code, out := do(t, s, "POST", "/v1/send", token, validBody); code != 401 || errClass(out) != "unauthorized" {
			t.Fatalf("token %q: %d %v", token, code, out)
		}
	}
	if code, _ := do(t, s, "GET", "/v1/stats", "", ""); code != 401 {
		t.Fatalf("stats without token: %d", code)
	}
	if code, _ := do(t, s, "GET", "/healthz", "", ""); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	if sender.count() != 0 {
		t.Fatal("sent without a token")
	}
	// An empty configured token never authorizes.
	open := New(Config{Delivery: &email.Delivery{Sender: sender, Counters: &email.Counters{}}})
	if code, _ := do(t, open, "POST", "/v1/send", "", validBody); code != 401 {
		t.Fatalf("empty token: %d", code)
	}
}

func TestBadRequests(t *testing.T) {
	sender := &fakeSender{}
	s := newTestServer(sender)
	for name, tc := range map[string]struct {
		body  string
		code  int
		class string
	}{
		"not json":         {`{`, 400, "permanent"},
		"unknown field":    {`{"to":[{"address":"a@example.com"}],"subject":"x","html":"y","bcc":["x@example.com"]}`, 400, "permanent"},
		"trailing data":    {validBody + `{}`, 400, "permanent"},
		"bad recipient":    {`{"to":[{"address":"nope"}],"subject":"x","html":"y"}`, 422, "invalid_recipient"},
		"no subject":       {`{"to":[{"address":"a@example.com"}],"html":"y"}`, 400, "permanent"},
		"header injection": {`{"to":[{"address":"a@example.com"}],"subject":"x\r\nBcc: b@example.com","html":"y"}`, 400, "permanent"},
	} {
		code, out := do(t, s, "POST", "/v1/send", testToken, tc.body)
		if code != tc.code || errClass(out) != tc.class {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if sender.count() != 0 {
		t.Fatal("an invalid message was sent")
	}
	big := `{"to":[{"address":"a@example.com"}],"subject":"x","html":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
	if code, _ := do(t, s, "POST", "/v1/send", testToken, big); code != 400 {
		t.Fatalf("oversized body: %d", code)
	}
}

func TestProviderErrorsKeepTheirClass(t *testing.T) {
	for _, tc := range []struct {
		err   error
		code  int
		class string
	}{
		{email.Classify("brevo", email.ErrRateLimited, errors.New("429")), 429, "rate_limited"},
		{email.Classify("brevo", email.ErrQuotaExceeded, errors.New("credits")), 429, "quota_exceeded"},
		{email.Classify("brevo", email.ErrTemporary, errors.New("503")), 503, "temporary"},
		{email.Classify("brevo", email.ErrInvalidRecipient, errors.New("bad to")), 422, "invalid_recipient"},
		{email.Classify("brevo", email.ErrPermanent, errors.New("sender not verified")), 502, "permanent"},
		{errors.New("unclassified"), 502, "permanent"},
	} {
		code, out := do(t, newTestServer(&fakeSender{errs: []error{tc.err}}), "POST", "/v1/send", testToken, validBody)
		if code != tc.code || errClass(out) != tc.class {
			t.Errorf("%v: %d %v", tc.err, code, out)
		}
		var classified *email.Error
		if errors.As(tc.err, &classified) && out["error"].(map[string]any)["provider"] != "brevo" {
			t.Errorf("%v: provider %v", tc.err, out["error"])
		}
	}
}

func TestIdempotentRetriesAreNotSentTwice(t *testing.T) {
	sender := &fakeSender{errs: []error{email.Classify("brevo", email.ErrTemporary, errors.New("503"))}}
	s := newTestServer(sender)
	body := strings.Replace(validBody, `"category"`, `"idempotency_key":"cashback_paid:42","category"`, 1)
	// A failed send is not remembered: the retry goes out.
	if code, _ := do(t, s, "POST", "/v1/send", testToken, body); code != 503 {
		t.Fatalf("first: %d", code)
	}
	if code, out := do(t, s, "POST", "/v1/send", testToken, body); code != 200 || out["duplicate"] == true {
		t.Fatalf("retry: %d %v", code, out)
	}
	// A successful send is remembered: the next retry is answered from it.
	code, out := do(t, s, "POST", "/v1/send", testToken, body)
	if code != 200 || out["duplicate"] != true || out["provider_message_id"] != "m-1" {
		t.Fatalf("duplicate: %d %v", code, out)
	}
	if sender.count() != 2 {
		t.Fatalf("sent %d times", sender.count())
	}
	// Another key is sent.
	other := strings.Replace(body, "cashback_paid:42", "cashback_paid:43", 1)
	if code, out := do(t, s, "POST", "/v1/send", testToken, other); code != 200 || out["duplicate"] == true || sender.count() != 3 {
		t.Fatalf("other key: %d %v", code, out)
	}
}

func TestConcurrentSendOfTheSameKeyIsRejected(t *testing.T) {
	sender := &fakeSender{block: make(chan struct{})}
	s := newTestServer(sender)
	body := strings.Replace(validBody, `"category"`, `"idempotency_key":"k","category"`, 1)
	first := make(chan int)
	go func() {
		code, _ := do(t, s, "POST", "/v1/send", testToken, body)
		first <- code
	}()
	deadline := time.Now().Add(2 * time.Second)
	for s.cfg.Idempotency.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	code, out := do(t, s, "POST", "/v1/send", testToken, body)
	if code != 409 || errClass(out) != "temporary" {
		t.Fatalf("concurrent: %d %v", code, out)
	}
	close(sender.block)
	if code := <-first; code != 200 {
		t.Fatalf("first: %d", code)
	}
	if sender.count() != 1 {
		t.Fatalf("sent %d times", sender.count())
	}
}

func TestSendSurvivesTheCallerDisconnecting(t *testing.T) {
	sender := &fakeSender{}
	s := newTestServer(sender)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/v1/send", strings.NewReader(validBody)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testToken)
	s.ServeHTTP(httptest.NewRecorder(), req)
	if sender.count() != 1 || sender.ctxErrs[0] != nil {
		t.Fatalf("send must run on its own context: count %d", sender.count())
	}
}

func TestStats(t *testing.T) {
	sender := &fakeSender{}
	s := newTestServer(sender)
	do(t, s, "POST", "/v1/send", testToken, validBody)
	code, out := do(t, s, "GET", "/v1/stats", testToken, "")
	counts, _ := out["counts"].(map[string]any)
	if code != 200 || counts["brevo ok"] != float64(1) || out["routing"] != "failover" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestIdempotencyEvictsWhenFull(t *testing.T) {
	now := time.Unix(1000, 0)
	i := NewIdempotency(time.Hour, 2)
	i.now = func() time.Time { return now }
	for _, key := range []string{"a", "b"} {
		claim, _, _ := i.Claim(key)
		claim.Done(email.Result{Provider: "p"})
		claim.Release()
		now = now.Add(time.Second)
	}
	claim, _, ok := i.Claim("c")
	if !ok || claim == nil || i.Len() != 2 {
		t.Fatalf("len %d", i.Len())
	}
	if _, kept := i.entries["a"]; kept {
		t.Fatal("the oldest key must be evicted")
	}
	if _, cached, _ := i.Claim("b"); cached == nil {
		t.Fatal("a newer key must be kept")
	}
	now = now.Add(2 * time.Hour)
	if _, cached, ok := i.Claim("b"); cached != nil || !ok {
		t.Fatal("an expired key must be sendable again")
	}
}
