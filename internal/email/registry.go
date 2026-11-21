package email

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Env reads provider-specific configuration (environment variables). Each
// provider owns its keys, so adding a provider never touches shared config.
type Env func(key string) string

// String returns the trimmed value of key, or def when unset.
func (e Env) String(key, def string) string {
	if e == nil {
		return def
	}
	if v := strings.TrimSpace(e(key)); v != "" {
		return v
	}
	return def
}

// Int returns key as an int, def when unset, and an error when malformed.
func (e Env) Int(key string, def int) (int, error) {
	raw := e.String(key, "")
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	return v, nil
}

// Bool returns key as a bool, def when unset, and an error when malformed.
func (e Env) Bool(key string, def bool) (bool, error) {
	raw := e.String(key, "")
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def, fmt.Errorf("%s must be true or false, got %q", key, raw)
	}
	return v, nil
}

// Duration returns key as a Go duration ("15s"), def when unset.
func (e Env) Duration(key string, def time.Duration) (time.Duration, error) {
	raw := e.String(key, "")
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		return def, fmt.Errorf("%s must be a positive duration such as 15s, got %q", key, raw)
	}
	return v, nil
}

// Factory builds a provider from its configuration. It validates the
// configuration and must not contact the provider.
type Factory func(env Env) (Sender, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a provider selectable by EMAIL_PROVIDER=<name>. Providers
// call it from an init function. Registering a name twice panics.
func Register(name string, factory Factory) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || factory == nil {
		panic("email: Register needs a name and a factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("email: provider registered twice: " + name)
	}
	registry[name] = factory
}

// Providers lists the registered provider names.
func Providers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NewProvider builds the named provider.
func NewProvider(name string, env Env) (Sender, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		registered := Providers()
		if len(registered) == 0 {
			return nil, fmt.Errorf("email provider %q is not available: no providers are registered (import mail-router/internal/email/provider)", name)
		}
		return nil, fmt.Errorf("email provider %q is not registered (available: %s)", name, strings.Join(registered, ", "))
	}
	sender, err := factory(env)
	if err != nil {
		return nil, fmt.Errorf("email provider %q: %w", name, err)
	}
	return sender, nil
}
