// Copyright (c) Microsoft. All rights reserved.

// Package toolmiddleware shares tool wrappers and invocation metadata between
// agent middleware and tool execution.
package toolmiddleware

import (
	"context"

	"github.com/microsoft/agent-framework-go/tool"
)

// Wrapper is an internal run option for wrapping tools registered outside agent options.
// Tools already present in agent options are wrapped by the originating middleware.
type Wrapper func(tool.FuncTool) tool.FuncTool

// MAFValue implements agent.Option without depending on the agent package.
func (w Wrapper) MAFValue() any { return w }

type callIDKey struct{}

// WithCallID associates the current tool invocation's call ID with ctx.
func WithCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, callIDKey{}, callID)
}

// CallIDFromContext returns the call ID for the current tool invocation.
func CallIDFromContext(ctx context.Context) (string, bool) {
	callID, ok := ctx.Value(callIDKey{}).(string)
	return callID, ok
}
