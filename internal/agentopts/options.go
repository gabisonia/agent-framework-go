// Copyright (c) Microsoft. All rights reserved.

// Package agentopts shares internal run options between agents and middleware.
package agentopts

// NoSessionProvided marks a run whose automatically created session does not
// need default history. Middleware that reuses the session can override it.
type NoSessionProvided bool

// MAFValue implements agent.Option without depending on the agent package.
func (o NoSessionProvided) MAFValue() any { return bool(o) }
