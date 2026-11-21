package email

import (
	"context"
	"sync"
	"testing"
)

type recordingSender struct {
	mu   sync.Mutex
	name string
	sent []Message
	errs []error // returned in order, then nil
}

func (r *recordingSender) Send(_ context.Context, msg Message) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, msg)
	var err error
	if len(r.errs) > 0 {
		err, r.errs = r.errs[0], r.errs[1:]
	}
	if err != nil {
		return Result{Provider: r.name}, err
	}
	return Result{Provider: r.name, ProviderMessageID: "id"}, nil
}

func registerTestProvider(t *testing.T, name string, sender Sender, factoryErr error) {
	t.Helper()
	registryMu.Lock()
	registry[name] = func(Env) (Sender, error) { return sender, factoryErr }
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, name)
		registryMu.Unlock()
	})
}

func testMessage() Message {
	return Message{
		From:     Address{Name: "Hoàn Xu", Address: "noreply@hoanxu.com"},
		To:       To("user@example.com"),
		Subject:  "Chào",
		HTML:     "<p>Xin <b>chào</b></p>",
		Text:     "Xin chào",
		Category: "welcome",
	}
}
