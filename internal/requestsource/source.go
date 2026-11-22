package requestsource

import "context"

type key struct{}

func WithMCP(ctx context.Context) context.Context {
	return context.WithValue(ctx, key{}, "mcp")
}

func FromContext(ctx context.Context) *string {
	if source, ok := ctx.Value(key{}).(string); ok {
		return &source
	}
	return nil
}
