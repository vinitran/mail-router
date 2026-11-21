package email

import (
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strings"
	"time"
)

// DefaultProvider is the provider used when EMAIL_PROVIDER is unset. It is
// what api-core used before sending moved to the mail-router.
const DefaultProvider = "smtp"

// DefaultFromName is the sender name when neither the request nor
// EMAIL_FROM_NAME sets one.
const DefaultFromName = "Hoàn Xu"

// Settings is the provider-independent configuration. Provider-specific keys
// (EMAIL_SMTP_*, RESEND_*, BREVO_*, SENDGRID_*, SES_*/AWS_*) are read
// through Env by each provider.
type Settings struct {
	// Provider is EMAIL_PROVIDER: one provider name, or a comma-separated
	// list with optional weights ("brevo:3,sendgrid,resend").
	Provider string
	// Fallback is EMAIL_PROVIDER_FALLBACK: providers tried, in order, only
	// after every Provider failed. Optional.
	Fallback string
	// Routing is EMAIL_ROUTING: RoutingFailover (default) or
	// RoutingRoundRobin. See Router.
	Routing string
	// Cooldown is EMAIL_PROVIDER_COOLDOWN; <= 0 uses DefaultCooldown.
	Cooldown time.Duration

	RetryAttempts      int           // EMAIL_RETRY_ATTEMPTS, total attempts per provider; <= 1 disables
	RetryDelay         time.Duration // EMAIL_RETRY_DELAY, first backoff
	RateLimitPerSecond float64       // EMAIL_RATE_LIMIT_PER_SECOND per provider; 0 disables

	Env  Env                              // provider-specific keys
	Logf func(format string, args ...any) // default log.Printf
}

// Delivery is the Sender chain built from Settings.
type Delivery struct {
	Sender    Sender
	Counters  *Counters
	Providers []string // usable providers, primaries first
	Routing   string
}

// Build creates the Sender chain: Instrument(Router(Retry(RateLimit(provider))...)).
//
// It fails when no EMAIL_PROVIDER entry can be built. Problems that leave
// at least one primary usable (a broken list entry or fallback, an unknown
// EMAIL_ROUTING) are returned as warnings next to a working Delivery.
func Build(settings Settings) (*Delivery, []error, error) {
	logf := settings.Logf
	if logf == nil {
		logf = log.Printf
	}
	var warnings []error
	primarySpecs, err := ParseProviders(settings.Provider)
	if err != nil {
		return nil, nil, err
	}
	if len(primarySpecs) == 0 {
		primarySpecs = []ProviderSpec{{Name: DefaultProvider, Weight: 1}}
	}
	fallbackSpecs, err := ParseProviders(settings.Fallback)
	if err != nil {
		warnings = append(warnings, fmt.Errorf("fallback disabled: %w", err))
		fallbackSpecs = nil
	}
	routing := strings.ToLower(strings.TrimSpace(settings.Routing))
	switch routing {
	case "", RoutingFailover:
		routing = RoutingFailover
	case RoutingRoundRobin, "roundrobin", "round-robin":
		routing = RoutingRoundRobin
	default:
		warnings = append(warnings, fmt.Errorf("EMAIL_ROUTING %q is not %s or %s; using %s", settings.Routing, RoutingFailover, RoutingRoundRobin, RoutingFailover))
		routing = RoutingFailover
	}

	delivery := &Delivery{Counters: &Counters{}, Routing: routing}
	seen := map[string]bool{}
	var primary, fallback []Route
	var primaryErrs []error
	for _, spec := range primarySpecs {
		seen[spec.Name] = true
		sender, err := buildChain(spec.Name, settings)
		if err != nil {
			primaryErrs = append(primaryErrs, err)
			continue
		}
		primary = append(primary, Route{Name: spec.Name, Sender: sender, Weight: spec.Weight})
		delivery.Providers = append(delivery.Providers, spec.Name)
	}
	if len(primary) == 0 {
		return nil, warnings, errors.Join(primaryErrs...)
	}
	for _, err := range primaryErrs {
		warnings = append(warnings, fmt.Errorf("provider disabled: %w", err))
	}
	for _, spec := range fallbackSpecs {
		if seen[spec.Name] {
			continue
		}
		seen[spec.Name] = true
		sender, err := buildChain(spec.Name, settings)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("fallback disabled: %w", err))
			continue
		}
		fallback = append(fallback, Route{Name: spec.Name, Sender: sender})
		delivery.Providers = append(delivery.Providers, spec.Name)
	}
	router := Router(primary, fallback, RouterOptions{Routing: routing, Cooldown: settings.Cooldown, Logf: logf})
	delivery.Sender = Instrument(router, LogFailures(logf), delivery.Counters)
	return delivery, warnings, nil
}

func buildChain(name string, settings Settings) (Sender, error) {
	provider, err := NewProvider(name, settings.Env)
	if err != nil {
		return nil, err
	}
	return Retry(RateLimit(provider, settings.RateLimitPerSecond), settings.RetryAttempts, settings.RetryDelay), nil
}

// Validate rejects messages no provider should send. Header injection
// through a subject, address or header is rejected outright. The returned
// error is classified (ErrInvalidRecipient or ErrPermanent).
func Validate(msg Message) error {
	if len(msg.To) == 0 {
		return Classify("", ErrPermanent, errors.New("at least one recipient is required"))
	}
	if len(msg.To) > 50 {
		return Classify("", ErrPermanent, errors.New("at most 50 recipients per message"))
	}
	for _, to := range msg.To {
		if !validAddress(to) {
			return Classify("", ErrInvalidRecipient, errors.New("invalid recipient"))
		}
	}
	if !validAddress(msg.From) {
		return Classify("", ErrPermanent, errors.New("invalid or missing sender address"))
	}
	if !msg.ReplyTo.IsZero() && !validAddress(msg.ReplyTo) {
		return Classify("", ErrPermanent, errors.New("invalid reply-to address"))
	}
	if strings.TrimSpace(msg.Subject) == "" {
		return Classify("", ErrPermanent, errors.New("email subject is required"))
	}
	if msg.HTML == "" && msg.Text == "" {
		return Classify("", ErrPermanent, errors.New("email html or text is required"))
	}
	if strings.ContainsAny(msg.Subject+msg.Category+msg.IdempotencyKey, "\r\n") {
		return Classify("", ErrPermanent, errors.New("email header contains a line break"))
	}
	for key, value := range msg.Headers {
		if key == "" || strings.ContainsAny(key, "\r\n: ") || strings.ContainsAny(value, "\r\n") {
			return Classify("", ErrPermanent, fmt.Errorf("invalid email header %q", key))
		}
	}
	for key, value := range msg.Tags {
		if key == "" || strings.ContainsAny(key+value, "\r\n") {
			return Classify("", ErrPermanent, fmt.Errorf("invalid email tag %q", key))
		}
	}
	return nil
}

func validAddress(a Address) bool {
	if a.IsZero() || strings.ContainsAny(a.Address+a.Name, "\r\n") {
		return false
	}
	parsed, err := mail.ParseAddress(a.Address)
	return err == nil && parsed.Address == strings.TrimSpace(a.Address)
}
