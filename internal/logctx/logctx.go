// Package logctx carries a request-scoped *slog.Logger in a context.Context.
package logctx

import (
	"context"
	"log/slog"
)

type key struct{}

// With returns a copy of ctx that carries logger.
func With(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, key{}, logger)
}

// From returns the logger stored in ctx, or fallback if there is none.
func From(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(key{}).(*slog.Logger); ok {
		return l
	}
	return fallback
}
