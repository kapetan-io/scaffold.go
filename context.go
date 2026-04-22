package scaffold

import (
	"context"
	"log/slog"
)

type configCtxKey struct{}
type secretsCtxKey struct{}
type loggerCtxKey struct{}

// GetConfig returns the ConfigProvider scaffold attached to ctx during the
// lifecycle. It returns nil when ctx was not enriched by scaffold (e.g. a
// per-request context or a context constructed outside the Start/Serve
// pipeline).
func GetConfig(ctx context.Context) ConfigProvider {
	v, _ := ctx.Value(configCtxKey{}).(ConfigProvider)
	return v
}

// GetSecrets returns the secrets ConfigProvider scaffold attached to ctx
// during the lifecycle. It returns nil when ctx was not enriched by scaffold.
func GetSecrets(ctx context.Context) ConfigProvider {
	v, _ := ctx.Value(secretsCtxKey{}).(ConfigProvider)
	return v
}

// GetLogger returns the *slog.Logger scaffold attached to ctx during the
// lifecycle. It returns nil when ctx was not enriched by scaffold.
func GetLogger(ctx context.Context) *slog.Logger {
	v, _ := ctx.Value(loggerCtxKey{}).(*slog.Logger)
	return v
}

// withConfig returns ctx with c attached under the private config key.
// The lifecycle orchestrator uses it to enrich OnStart and OnStop
// contexts; GetConfig is the public accessor counterpart.
func withConfig(ctx context.Context, c ConfigProvider) context.Context {
	return context.WithValue(ctx, configCtxKey{}, c)
}

func withSecrets(ctx context.Context, c ConfigProvider) context.Context {
	return context.WithValue(ctx, secretsCtxKey{}, c)
}

func withLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey{}, l)
}
