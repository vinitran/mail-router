package email

import (
	"context"
	"errors"
)

// Error classes. Providers map their native errors onto exactly one of them
// with Classify, so retry and failover logic works the same for every
// provider. Test with errors.Is(err, email.ErrRateLimited).
var (
	// ErrRateLimited: too many requests right now; retrying later works.
	ErrRateLimited = errors.New("email: rate limited")
	// ErrQuotaExceeded: a daily/monthly sending quota is used up, or sending
	// is paused; this provider will not accept mail for a while.
	ErrQuotaExceeded = errors.New("email: quota exceeded")
	// ErrInvalidRecipient: the recipient address is rejected.
	ErrInvalidRecipient = errors.New("email: invalid recipient")
	// ErrTemporary: network, timeout or 5xx-style failure; retrying may work.
	ErrTemporary = errors.New("email: temporary failure")
	// ErrPermanent: the request will not succeed as is (auth, unverified
	// domain, malformed message, ...).
	ErrPermanent = errors.New("email: permanent failure")
)

// Error is a provider error with its class. Error() is the underlying
// message unchanged, so logs read as before.
type Error struct {
	Class    error // one of the Err* classes above
	Provider string
	Err      error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Class.Error()
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Is matches the error class as well as the wrapped chain.
func (e *Error) Is(target error) bool { return target == e.Class }

// Classify wraps err with a class. A nil err stays nil. An error that is
// already classified keeps its class.
func Classify(provider string, class error, err error) error {
	if err == nil {
		return nil
	}
	var existing *Error
	if errors.As(err, &existing) {
		return err
	}
	return &Error{Class: class, Provider: provider, Err: err}
}

// ClassOf returns the class of err, ErrTemporary for a context timeout or
// cancellation, and nil for an unclassified error.
func ClassOf(err error) error {
	for _, class := range []error{ErrRateLimited, ErrQuotaExceeded, ErrInvalidRecipient, ErrTemporary, ErrPermanent} {
		if errors.Is(err, class) {
			return class
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrTemporary
	}
	return nil
}

// IsRetryable reports whether sending the same message again later through
// the same provider may succeed.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrTemporary)
}

// ShouldFailover reports whether another provider may accept the message.
func ShouldFailover(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrTemporary)
}

// ClassName is a short label for logs and metrics.
func ClassName(err error) string {
	switch ClassOf(err) {
	case nil:
		if err == nil {
			return "ok"
		}
		return "unclassified"
	case ErrRateLimited:
		return "rate_limited"
	case ErrQuotaExceeded:
		return "quota_exceeded"
	case ErrInvalidRecipient:
		return "invalid_recipient"
	case ErrTemporary:
		return "temporary"
	default:
		return "permanent"
	}
}
