package scaffold

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileConfigProvider serves configuration from a directory of per-key files,
// the layout used by Kubernetes ConfigMap and Secret volume mounts. Each
// lookup reads the file {Dir}/{key} on every call so that atomically-rotated
// mounts are picked up without reload logic.
type FileConfigProvider struct {
	Dir    string
	Logger *slog.Logger
}

func (p *FileConfigProvider) readKey(key string) (string, bool, error) {
	if err := validateKey(key); err != nil {
		return "", false, err
	}
	b, err := os.ReadFile(filepath.Join(p.Dir, key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("scaffold: config key %q: read failed: %w", key, err)
	}
	return trimOneTrailingNewline(string(b)), true, nil
}

// validateKey rejects keys that would escape the configured directory or
// that are not a single clean path component.
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("scaffold: config key %q is invalid", key)
	}
	if strings.ContainsRune(key, '/') || strings.ContainsRune(key, filepath.Separator) {
		return fmt.Errorf("scaffold: config key %q must not contain path separators", key)
	}
	if key == "." || key == ".." {
		return fmt.Errorf("scaffold: config key %q is invalid", key)
	}
	if filepath.Clean(key) != key {
		return fmt.Errorf("scaffold: config key %q is not a clean path component", key)
	}
	return nil
}

func trimOneTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}

func (p *FileConfigProvider) String(key string) (string, error) {
	v, ok, err := p.readKey(key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", notFoundError(key)
	}
	return v, nil
}

func (p *FileConfigProvider) StringOr(key, fallback string) string {
	v, ok, err := p.readKey(key)
	if err != nil || !ok {
		return fallback
	}
	return v
}

func (p *FileConfigProvider) Int(key string) (int, error) {
	v, ok, err := p.readKey(key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, notFoundError(key)
	}
	return parseInt(key, v)
}

func (p *FileConfigProvider) IntOr(key string, fallback int) int {
	v, ok, err := p.readKey(key)
	if err != nil || !ok {
		return fallback
	}
	parsed, perr := parseInt(key, v)
	if perr != nil {
		logParseFailure(p.Logger, key, v, "int", perr)
		return fallback
	}
	return parsed
}

func (p *FileConfigProvider) Int64(key string) (int64, error) {
	v, ok, err := p.readKey(key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, notFoundError(key)
	}
	return parseInt64(key, v)
}

func (p *FileConfigProvider) Int64Or(key string, fallback int64) int64 {
	v, ok, err := p.readKey(key)
	if err != nil || !ok {
		return fallback
	}
	parsed, perr := parseInt64(key, v)
	if perr != nil {
		logParseFailure(p.Logger, key, v, "int64", perr)
		return fallback
	}
	return parsed
}

func (p *FileConfigProvider) Float64(key string) (float64, error) {
	v, ok, err := p.readKey(key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, notFoundError(key)
	}
	return parseFloat64(key, v)
}

func (p *FileConfigProvider) Float64Or(key string, fallback float64) float64 {
	v, ok, err := p.readKey(key)
	if err != nil || !ok {
		return fallback
	}
	parsed, perr := parseFloat64(key, v)
	if perr != nil {
		logParseFailure(p.Logger, key, v, "float64", perr)
		return fallback
	}
	return parsed
}

func (p *FileConfigProvider) Bool(key string) (bool, error) {
	v, ok, err := p.readKey(key)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, notFoundError(key)
	}
	return parseBool(key, v)
}

func (p *FileConfigProvider) BoolOr(key string, fallback bool) bool {
	v, ok, err := p.readKey(key)
	if err != nil || !ok {
		return fallback
	}
	parsed, perr := parseBool(key, v)
	if perr != nil {
		logParseFailure(p.Logger, key, v, "bool", perr)
		return fallback
	}
	return parsed
}

func (p *FileConfigProvider) Duration(key string) (time.Duration, error) {
	v, ok, err := p.readKey(key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, notFoundError(key)
	}
	return parseDuration(key, v)
}

func (p *FileConfigProvider) DurationOr(key string, fallback time.Duration) time.Duration {
	v, ok, err := p.readKey(key)
	if err != nil || !ok {
		return fallback
	}
	parsed, perr := parseDuration(key, v)
	if perr != nil {
		logParseFailure(p.Logger, key, v, "duration", perr)
		return fallback
	}
	return parsed
}
