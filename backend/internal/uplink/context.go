// Package uplink isolates only a module's outer carrier connections.
package uplink

import (
	"context"
	"regexp"
)

type contextKey struct{}

var identifier = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ValidID(id string) bool { return identifier.MatchString(id) }
func WithNetwork(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}
func Network(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}
