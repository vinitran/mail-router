// Command mail-router is the HTTP service that delivers email for api-core
// (hoanxu.com, hoantien.work) through one or more providers. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mail-router/internal/email"
	_ "mail-router/internal/email/provider" // registers smtp, resend, brevo, sendgrid, ses, log, noop
	"mail-router/internal/server"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "GET /healthz on MAIL_ROUTER_ADDR and exit 0 when healthy (Docker HEALTHCHECK)")
	flag.Parse()
	addr := envString("MAIL_ROUTER_ADDR", ":8080")
	if *healthcheck {
		os.Exit(checkHealth(addr))
	}
	if err := run(addr); err != nil {
		log.Fatalf("mail-router: %v", err)
	}
}

func run(addr string) error {
	env := email.Env(os.Getenv)
	token := strings.TrimSpace(os.Getenv("MAIL_ROUTER_TOKEN"))
	if len(token) < 32 {
		return errors.New("MAIL_ROUTER_TOKEN must be set to at least 32 characters (openssl rand -hex 32)")
	}
	cooldown, err := env.Duration("EMAIL_PROVIDER_COOLDOWN", 0)
	if err != nil {
		return err
	}
	retryAttempts, err := env.Int("EMAIL_RETRY_ATTEMPTS", 1)
	if err != nil {
		return err
	}
	retryDelay, err := env.Duration("EMAIL_RETRY_DELAY", 500*time.Millisecond)
	if err != nil {
		return err
	}
	rate := 0.0
	if raw := env.String("EMAIL_RATE_LIMIT_PER_SECOND", ""); raw != "" {
		if rate, err = strconv.ParseFloat(raw, 64); err != nil || rate < 0 {
			return fmt.Errorf("EMAIL_RATE_LIMIT_PER_SECOND must be a number >= 0, got %q", raw)
		}
	}
	sendTimeout, err := env.Duration("MAIL_ROUTER_SEND_TIMEOUT", 60*time.Second)
	if err != nil {
		return err
	}
	idemTTL, err := env.Duration("MAIL_ROUTER_IDEMPOTENCY_TTL", 24*time.Hour)
	if err != nil {
		return err
	}

	delivery, warnings, err := email.Build(email.Settings{
		Provider:           env.String("EMAIL_PROVIDER", email.DefaultProvider),
		Fallback:           env.String("EMAIL_PROVIDER_FALLBACK", ""),
		Routing:            env.String("EMAIL_ROUTING", ""),
		Cooldown:           cooldown,
		RetryAttempts:      retryAttempts,
		RetryDelay:         retryDelay,
		RateLimitPerSecond: rate,
		Env:                env,
		Logf:               log.Printf,
	})
	if err != nil {
		return fmt.Errorf("no usable email provider: %w", err)
	}
	for _, w := range warnings {
		log.Printf("mail-router: configuration problem: %v", w)
	}

	// The sender address: EMAIL_FROM_ADDRESS, the legacy EMAIL_FROM_EMAIL,
	// then the SMTP login, as api-core resolved it.
	from := email.Address{
		Name:    env.String("EMAIL_FROM_NAME", email.DefaultFromName),
		Address: env.String("EMAIL_FROM_ADDRESS", env.String("EMAIL_FROM_EMAIL", "")),
	}
	if from.Address == "" && strings.Contains(env.String("EMAIL_SMTP_USERNAME", ""), "@") {
		from.Address = env.String("EMAIL_SMTP_USERNAME", "")
	}

	handler := server.New(server.Config{
		Token:       token,
		SendTimeout: sendTimeout,
		DefaultFrom: from,
		Delivery:    delivery,
		Idempotency: server.NewIdempotency(idemTTL, 0),
		Logf:        log.Printf,
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      sendTimeout + 30*time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	if from.Address == "" {
		log.Printf("mail-router: no EMAIL_FROM_ADDRESS: every request must set from")
	}
	log.Printf("mail-router: listening on %s; providers %s (%s)", addr, strings.Join(delivery.Providers, ", "), delivery.Routing)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("mail-router: shutting down, finishing sends in progress")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), sendTimeout+5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func checkHealth(addr string) int {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + host + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
