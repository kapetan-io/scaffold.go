package scaffold

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// ConfigProvider reads typed configuration values by string key. Plain
// variants return an error when the key is absent or the value fails to
// parse. Or variants return the fallback silently on a missing key and, on
// parse failure, log a warning through the provider's logger (if any)
// before returning the fallback.
type ConfigProvider interface {
	String(key string) (string, error)
	StringOr(key, fallback string) string
	Int(key string) (int, error)
	IntOr(key string, fallback int) int
	Int64(key string) (int64, error)
	Int64Or(key string, fallback int64) int64
	Float64(key string) (float64, error)
	Float64Or(key string, fallback float64) float64
	Bool(key string) (bool, error)
	BoolOr(key string, fallback bool) bool
	Duration(key string) (time.Duration, error)
	DurationOr(key string, fallback time.Duration) time.Duration
}

// EnvConfigProvider reads configuration from process environment variables.
// Values are read via os.LookupEnv on every call; a zero-value provider is
// valid and will simply report every key as missing when the environment
// has no matching variable.
type EnvConfigProvider struct {
	Logger *slog.Logger
}

func (p *EnvConfigProvider) lookup(key string) (string, bool) {
	return os.LookupEnv(key)
}

func (p *EnvConfigProvider) String(key string) (string, error) {
	v, ok := p.lookup(key)
	if !ok {
		return "", notFoundError(key)
	}
	return v, nil
}

func (p *EnvConfigProvider) StringOr(key, fallback string) string {
	v, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	return v
}

func (p *EnvConfigProvider) Int(key string) (int, error) {
	v, ok := p.lookup(key)
	if !ok {
		return 0, notFoundError(key)
	}
	return parseInt(key, v)
}

func (p *EnvConfigProvider) IntOr(key string, fallback int) int {
	v, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	parsed, err := parseInt(key, v)
	if err != nil {
		logParseFailure(p.Logger, key, v, "int", err)
		return fallback
	}
	return parsed
}

func (p *EnvConfigProvider) Int64(key string) (int64, error) {
	v, ok := p.lookup(key)
	if !ok {
		return 0, notFoundError(key)
	}
	return parseInt64(key, v)
}

func (p *EnvConfigProvider) Int64Or(key string, fallback int64) int64 {
	v, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	parsed, err := parseInt64(key, v)
	if err != nil {
		logParseFailure(p.Logger, key, v, "int64", err)
		return fallback
	}
	return parsed
}

func (p *EnvConfigProvider) Float64(key string) (float64, error) {
	v, ok := p.lookup(key)
	if !ok {
		return 0, notFoundError(key)
	}
	return parseFloat64(key, v)
}

func (p *EnvConfigProvider) Float64Or(key string, fallback float64) float64 {
	v, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	parsed, err := parseFloat64(key, v)
	if err != nil {
		logParseFailure(p.Logger, key, v, "float64", err)
		return fallback
	}
	return parsed
}

func (p *EnvConfigProvider) Bool(key string) (bool, error) {
	v, ok := p.lookup(key)
	if !ok {
		return false, notFoundError(key)
	}
	return parseBool(key, v)
}

func (p *EnvConfigProvider) BoolOr(key string, fallback bool) bool {
	v, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	parsed, err := parseBool(key, v)
	if err != nil {
		logParseFailure(p.Logger, key, v, "bool", err)
		return fallback
	}
	return parsed
}

func (p *EnvConfigProvider) Duration(key string) (time.Duration, error) {
	v, ok := p.lookup(key)
	if !ok {
		return 0, notFoundError(key)
	}
	return parseDuration(key, v)
}

func (p *EnvConfigProvider) DurationOr(key string, fallback time.Duration) time.Duration {
	v, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	parsed, err := parseDuration(key, v)
	if err != nil {
		logParseFailure(p.Logger, key, v, "duration", err)
		return fallback
	}
	return parsed
}

// notFoundError returns an error whose message names the missing key.
func notFoundError(key string) error {
	return fmt.Errorf("scaffold: config key %q not found", key)
}

// parseFailureError returns an error whose message names the key and a
// snippet of the offending value.
func parseFailureError(key, value, kind string, err error) error {
	return fmt.Errorf("scaffold: config key %q: failed to parse %q as %s: %w", key, value, kind, err)
}

func parseInt(key, v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, parseFailureError(key, v, "int", err)
	}
	return n, nil
}

func parseInt64(key, v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, parseFailureError(key, v, "int64", err)
	}
	return n, nil
}

func parseFloat64(key, v string) (float64, error) {
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, parseFailureError(key, v, "float64", err)
	}
	return n, nil
}

func parseBool(key, v string) (bool, error) {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, parseFailureError(key, v, "bool", err)
	}
	return b, nil
}

func parseDuration(key, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, parseFailureError(key, v, "duration", err)
	}
	return d, nil
}

func logParseFailure(log *slog.Logger, key, value, kind string, err error) {
	if log == nil {
		return
	}
	log.Warn("config parse failed; using fallback",
		"key", key,
		"value", value,
		"kind", kind,
		"error", err)
}
