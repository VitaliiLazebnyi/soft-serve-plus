package proto

import "context"

// ContextKeyUser is the context key for the user.
var ContextKeyUser = &struct{ string }{"user"}

// UserFromContext returns the user from the context.
func UserFromContext(ctx context.Context) User {
	if u, ok := ctx.Value(ContextKeyUser).(User); ok {
		return u
	}
	return nil
}

// WithUserContext returns a new context with the user.
func WithUserContext(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ContextKeyUser, u)
}
