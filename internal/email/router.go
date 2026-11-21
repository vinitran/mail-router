package email

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Routing modes (EMAIL_ROUTING).
const (
	// RoutingFailover sends every message through the first provider of
	// EMAIL_PROVIDER and moves to the next one only when it fails with an
	// error another provider may not share. This is the default.
	RoutingFailover = "failover"
	// RoutingRoundRobin spreads messages over the EMAIL_PROVIDER providers
	// in proportion to their weights (smooth weighted round robin), so the
	// free quotas of several providers add up. A message whose provider
	// fails is still sent through the others.
	RoutingRoundRobin = "round_robin"
)

// DefaultCooldown is how long a provider that reported ErrQuotaExceeded is
// skipped when EMAIL_PROVIDER_COOLDOWN is unset.
const DefaultCooldown = 30 * time.Minute

// Route is one provider a Router can send through.
type Route struct {
	Name   string
	Sender Sender
	// Weight is the provider's share of messages under RoutingRoundRobin
	// (values below 1 count as 1). It is ignored under RoutingFailover.
	Weight int
}

// RouterOptions configures a Router.
type RouterOptions struct {
	Routing string // RoutingFailover (default) or RoutingRoundRobin
	// Cooldown is how long a provider that reported ErrQuotaExceeded is tried
	// only after every other provider. <= 0 uses DefaultCooldown.
	Cooldown time.Duration
	Logf     func(format string, args ...any)
	now      func() time.Time
}

// Router sends each message through one of primary and, when that provider
// fails with an error another provider may not share (ShouldFailover:
// anything but an invalid recipient), through the remaining primaries and
// then through fallback, in order. An invalid-recipient error is returned
// at once.
//
// Under RoutingRoundRobin the first provider rotates by weight; under
// RoutingFailover it is always primary[0]. Fallback providers are never in
// the rotation: they only take over when every primary failed.
//
// A provider that reported ErrQuotaExceeded is moved behind the others for
// the cooldown, so a used-up daily quota costs one failed request per
// cooldown instead of one per message. When every provider is cooling down
// they are still tried, in the usual order.
//
// A temporary error can be ambiguous (a timeout after the provider accepted
// the message), so moving on can duplicate an email, just as the existing
// at-least-once consumers can.
//
// With one route in total the Sender is returned unwrapped.
func Router(primary, fallback []Route, opts RouterOptions) Sender {
	routes := make([]*route, 0, len(primary)+len(fallback))
	for i, r := range append(append([]Route{}, primary...), fallback...) {
		if r.Sender == nil {
			continue
		}
		weight := r.Weight
		if weight < 1 {
			weight = 1
		}
		routes = append(routes, &route{Route: r, weight: weight, inRotation: i < len(primary)})
	}
	if len(routes) == 1 {
		return routes[0].Sender
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = DefaultCooldown
	}
	return &router{routes: routes, opts: opts}
}

type route struct {
	Route
	weight     int
	inRotation bool

	// guarded by router.mu
	current      int       // smooth weighted round robin state
	coolingUntil time.Time // set on ErrQuotaExceeded
}

type router struct {
	routes []*route
	opts   RouterOptions
	mu     sync.Mutex
}

// order is the sequence in which providers are tried for one message.
func (r *router) order() []*route {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.now()
	var first *route
	if r.opts.Routing == RoutingRoundRobin {
		first = r.pickLocked(now)
	}
	ready := make([]*route, 0, len(r.routes))
	var cooling []*route
	if first != nil {
		ready = append(ready, first)
	}
	for _, rt := range r.routes {
		switch {
		case rt == first:
		case now.Before(rt.coolingUntil):
			cooling = append(cooling, rt)
		default:
			ready = append(ready, rt)
		}
	}
	return append(ready, cooling...)
}

// pickLocked runs one step of smooth weighted round robin over the rotation
// providers that are not cooling down (nginx's algorithm: weights 3,1,1
// give a a b a c, not a a a b c).
func (r *router) pickLocked(now time.Time) *route {
	var best *route
	total := 0
	for _, rt := range r.routes {
		if !rt.inRotation || now.Before(rt.coolingUntil) {
			continue
		}
		rt.current += rt.weight
		total += rt.weight
		if best == nil || rt.current > best.current {
			best = rt
		}
	}
	if best != nil {
		best.current -= total
	}
	return best
}

func (r *router) coolDown(rt *route) {
	r.mu.Lock()
	rt.coolingUntil = r.opts.now().Add(r.opts.Cooldown)
	r.mu.Unlock()
}

func (r *router) Send(ctx context.Context, msg Message) (Result, error) {
	var (
		result Result
		err    error
		tried  []string // "name class", for the final error
	)
	for _, rt := range r.order() {
		if len(tried) > 0 {
			// Provider error text can contain the recipient; log the class only.
			r.opts.Logf("email: provider %s, sending %q via %s", tried[len(tried)-1], msg.Category, rt.Name)
		}
		result, err = rt.Sender.Send(ctx, msg)
		if err == nil {
			return result, nil
		}
		tried = append(tried, rt.Name+" "+ClassName(err))
		if ClassOf(err) == ErrQuotaExceeded {
			r.coolDown(rt)
		}
		if !ShouldFailover(err) || ctx.Err() != nil {
			break
		}
	}
	if len(tried) == 1 {
		return result, err
	}
	return result, fmt.Errorf("%w (providers tried: %s)", err, strings.Join(tried, ", "))
}

// ParseProviders reads EMAIL_PROVIDER / EMAIL_PROVIDER_FALLBACK:
// comma-separated provider names, each with an optional ":weight", e.g.
// "brevo:3, sendgrid, resend:1". Names are lower-cased; a name listed twice
// keeps its first entry.
func ParseProviders(raw string) ([]ProviderSpec, error) {
	var (
		specs []ProviderSpec
		seen  = map[string]bool{}
	)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, weightRaw, hasWeight := strings.Cut(part, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		weight := 1
		if hasWeight {
			w, err := strconv.Atoi(strings.TrimSpace(weightRaw))
			if err != nil || w < 1 || w > 1000 {
				return nil, fmt.Errorf("email provider %q: weight must be a whole number from 1 to 1000, got %q", name, weightRaw)
			}
			weight = w
		}
		if name == "" {
			return nil, fmt.Errorf("email provider list %q has an entry without a name", raw)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		specs = append(specs, ProviderSpec{Name: name, Weight: weight})
	}
	return specs, nil
}

// ProviderSpec is one entry of a provider list.
type ProviderSpec struct {
	Name   string
	Weight int
}
