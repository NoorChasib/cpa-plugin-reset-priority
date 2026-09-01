package hostapi

import (
	"context"
	"strings"
)

type hostCallbackIDContextKey struct{}

// WithHostCallbackID associates an inbound native request's CPA callback ID
// with ctx. The ID is wire metadata, not a credential or authorization value.
// Bridge.HTTPDo copies it into host_callback_id so CPA can resolve the original
// request's cancellation context before issuing provider HTTP.
func WithHostCallbackID(ctx context.Context, callbackID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	callbackID = strings.TrimSpace(callbackID)
	if callbackID == "" {
		return ctx
	}
	return context.WithValue(ctx, hostCallbackIDContextKey{}, callbackID)
}

// HostCallbackIDFromContext returns the CPA callback ID associated with ctx.
func HostCallbackIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	callbackID, _ := ctx.Value(hostCallbackIDContextKey{}).(string)
	return strings.TrimSpace(callbackID)
}
