package llm

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ProviderRetryAfter reports the delay a provider asked for via a
// Retry-After response header, when the error carries one. It is populated
// for both HTTP-error shapes this package produces (openAIHTTPError and
// ollamaHTTPError), so it works uniformly across every dialect.
//
// A caller that wants to retry a 429 (see IsRetryableProviderError) must
// honor this delay — or apply its own conservative backoff when ok is false
// — rather than resending immediately. Resending a rate-limited or
// quota-exhausted request with no delay cannot succeed and only spends
// another billable request.
func ProviderRetryAfter(err error) (time.Duration, bool) {
	var openAIErr *openAIHTTPError
	if errors.As(err, &openAIErr) && openAIErr.RetryAfter > 0 {
		return openAIErr.RetryAfter, true
	}
	var ollamaErr *ollamaHTTPError
	if errors.As(err, &ollamaErr) && ollamaErr.RetryAfter > 0 {
		return ollamaErr.RetryAfter, true
	}
	return 0, false
}

// IsRetryableProviderError reports whether a dispatched provider request
// failed in a way that retrying the same request could plausibly resolve.
// It is a superset of IsRetryableTransport that additionally admits 429
// (rate limit / quota exhaustion).
//
// The 429 case is deliberately not folded into IsRetryableTransport: that
// predicate signals "safe to resend now with the same request," which is
// true for a dropped connection or a provider 5xx but never true for a rate
// limit — an immediate resend on a 429 wastes another billable request
// against a quota that is, by definition, already exhausted or throttled. A
// caller that receives true here for a 429 must consult ProviderRetryAfter
// (or otherwise back off) before resending; it must not treat this the same
// as an IsRetryableTransport-style immediate retry.
//
// Every other 4xx status (401, 403, 404, 400, ...) returns false: the exact
// same request cannot succeed without changing credentials, the endpoint, or
// the payload, so retrying it is never productive.
func IsRetryableProviderError(err error) bool {
	if IsRetryableTransport(err) {
		return true
	}
	status, ok := ProviderHTTPStatus(err)
	return ok && status == http.StatusTooManyRequests
}

// parseRetryAfter parses an HTTP Retry-After header value per RFC 9110
// §10.2.3: either a non-negative integer count of delta-seconds, or an
// HTTP-date. now is the reference time an HTTP-date is measured against;
// tests pass a fixed time so header parsing never races the wall clock.
//
// A malformed, empty, or negative header reports ok=false: an unparsable
// hint is not the same as "the provider asked for a zero-second wait," and
// must not be treated as one by a caller composing its own backoff.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(header); err == nil {
		if delta := when.Sub(now); delta > 0 {
			return delta, true
		}
		// The provider's requested time has already passed: no additional
		// wait is needed, but the header itself was present and valid.
		return 0, true
	}
	return 0, false
}
