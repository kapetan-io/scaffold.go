package scaffold

import (
	"log/slog"
	"time"
)

// MapConfigProvider serves configuration from an in-memory map. A zero-value
// provider is valid; a nil Values map is treated as empty and reports every
// lookup as missing.
type MapConfigProvider struct {
	Values map[string]string
	Logger *slog.Logger
}

func (p *MapConfigProvider) lookup(key string) (string, bool) {
	if p.Values == nil {
		return "", false
	}
	v, ok := p.Values[key]
	return v, ok
}

func (p *MapConfigProvider) String(key string) (string, error) {
	v, ok := p.lookup(key)
	if !ok {
		return "", notFoundError(key)
	}
	return v, nil
}

func (p *MapConfigProvider) StringOr(key, fallback string) string {
	v, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	return v
}

func (p *MapConfigProvider) Int(key string) (int, error) {
	v, ok := p.lookup(key)
	if !ok {
		return 0, notFoundError(key)
	}
	return parseInt(key, v)
}

func (p *MapConfigProvider) IntOr(key string, fallback int) int {
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

func (p *MapConfigProvider) Int64(key string) (int64, error) {
	v, ok := p.lookup(key)
	if !ok {
		return 0, notFoundError(key)
	}
	return parseInt64(key, v)
}

func (p *MapConfigProvider) Int64Or(key string, fallback int64) int64 {
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

func (p *MapConfigProvider) Float64(key string) (float64, error) {
	v, ok := p.lookup(key)
	if !ok {
		return 0, notFoundError(key)
	}
	return parseFloat64(key, v)
}

func (p *MapConfigProvider) Float64Or(key string, fallback float64) float64 {
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

func (p *MapConfigProvider) Bool(key string) (bool, error) {
	v, ok := p.lookup(key)
	if !ok {
		return false, notFoundError(key)
	}
	return parseBool(key, v)
}

func (p *MapConfigProvider) BoolOr(key string, fallback bool) bool {
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

func (p *MapConfigProvider) Duration(key string) (time.Duration, error) {
	v, ok := p.lookup(key)
	if !ok {
		return 0, notFoundError(key)
	}
	return parseDuration(key, v)
}

func (p *MapConfigProvider) DurationOr(key string, fallback time.Duration) time.Duration {
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
