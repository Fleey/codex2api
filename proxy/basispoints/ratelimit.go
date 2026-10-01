package basispoints

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RateLimitMessage is the client-facing text for a Basispoints rate limit.
// A retry delay is appended when the provider gave one.
const RateLimitMessage = "Rate limit reached on the Basispoints upstream."

// retryHint matches the delay in a provider "Please try again in 74ms" hint.
var retryHint = regexp.MustCompile(`(?i)try again in ([0-9]{1,6}(?:\.[0-9]{1,3})?)(ms|s)\b`)

// terminalErrorSource returns the error object of a provider failure: the
// error of a failed response, the error of an error event, or the payload.
func terminalErrorSource(payload object) object {
	var source object
	if response, ok := payload["response"].(object); ok {
		source, _ = response["error"].(object)
	}
	if source == nil {
		source, _ = payload["error"].(object)
	}
	if source == nil {
		source = payload
	}
	return source
}

// RateLimitNotice returns a client-safe message for a provider rate limit and
// the retry delay it names (0 when unknown), or "" when body is not a rate
// limit. An HTTP 429 counts as one even without a rate-limit code. Only the
// delay is kept from the provider error, since its message names the provider
// organization and quota figures.
func RateLimitNotice(body []byte, header http.Header, status int) (string, time.Duration) {
	var payload object
	_ = decode(body, &payload)
	source := terminalErrorSource(payload)
	if status != http.StatusTooManyRequests && !isRateLimit(source) {
		return "", 0
	}
	return rateLimitNotice(source, header)
}

// isRateLimit also counts slow_down, the code the shared tokens-per-minute
// budget is reported under at times; waiting it out works the same way.
func isRateLimit(source object) bool {
	code := strings.ToLower(text(source["code"]))
	return strings.Contains(code, "rate_limit") || code == "slow_down" ||
		strings.Contains(strings.ToLower(text(source["type"])), "rate_limit")
}

func rateLimitNotice(source object, header http.Header) (string, time.Duration) {
	if delay, ok := retryDelay(source, header); ok {
		return RateLimitMessage + " Please try again in " + formatDelay(delay) + ".", delay
	}
	return RateLimitMessage, 0
}

// retryDelay reads the provider's retry delay from the response headers, the
// headers BPS copies into the error object, or the message hint, in that order.
func retryDelay(source object, header http.Header) (time.Duration, bool) {
	embedded, _ := source["headers"].(object)
	lookup := func(name string) string {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
		switch value := embedded[name].(type) {
		case string:
			return strings.TrimSpace(value)
		case json.Number:
			return value.String()
		}
		return ""
	}
	if ms, err := strconv.ParseFloat(lookup("retry-after-ms"), 64); err == nil && ms >= 0 && ms < 1e9 {
		return time.Duration(ms * float64(time.Millisecond)), true
	}
	if seconds, err := strconv.ParseFloat(lookup("retry-after"), 64); err == nil && seconds >= 0 && seconds < 1e6 {
		return time.Duration(seconds * float64(time.Second)), true
	}
	if match := retryHint.FindStringSubmatch(text(source["message"])); match != nil {
		value, _ := strconv.ParseFloat(match[1], 64)
		unit := time.Second
		if strings.EqualFold(match[2], "ms") {
			unit = time.Millisecond
		}
		return time.Duration(value * float64(unit)), true
	}
	return 0, false
}

func formatDelay(delay time.Duration) string {
	delay = delay.Round(time.Millisecond)
	if delay < time.Second {
		return fmt.Sprintf("%dms", delay.Milliseconds())
	}
	return strconv.FormatFloat(delay.Seconds(), 'f', -1, 64) + "s"
}
