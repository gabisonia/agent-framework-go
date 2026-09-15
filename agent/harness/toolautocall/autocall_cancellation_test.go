// Copyright (c) Microsoft. All rights reserved.

package toolautocall_test

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/toolautocall"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

func TestFunctionInvoking_RequestCancellation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cancelAt      string
		deadline      bool
		concurrent    bool
		toolError     error
		wantTools     int32
		wantProviders int
	}{
		{name: "before request", cancelAt: "before request", wantProviders: 0},
		{name: "before tools", cancelAt: "before tools", wantProviders: 1},
		{name: "before concurrent tools", cancelAt: "before tools", concurrent: true, wantProviders: 1},
		{name: "during first tool", cancelAt: "first tool", toolError: context.Canceled, wantTools: 1, wantProviders: 1},
		{name: "during successful first tool", cancelAt: "first tool", wantTools: 1, wantProviders: 1},
		{name: "during last tool", cancelAt: "last tool", toolError: context.Canceled, wantTools: 2, wantProviders: 1},
		{name: "during concurrent tools", cancelAt: "concurrent tools", concurrent: true, toolError: context.Canceled, wantTools: 2, wantProviders: 1},
		{name: "during successful concurrent tools", cancelAt: "concurrent tools", concurrent: true, wantTools: 2, wantProviders: 1},
		{name: "deadline during first tool", cancelAt: "first tool", deadline: true, toolError: context.DeadlineExceeded, wantTools: 1, wantProviders: 1},
		{name: "after tool results", cancelAt: "after results", wantTools: 2, wantProviders: 1},
		{name: "ordinary tool error", toolError: errors.New("temporary failure"), wantTools: 2, wantProviders: 2},
		{name: "independent tool cancellation", toolError: context.Canceled, wantTools: 2, wantProviders: 2},
		{name: "independent tool deadline", toolError: context.DeadlineExceeded, wantTools: 2, wantProviders: 2},
		{name: "independent concurrent cancellation", concurrent: true, toolError: context.Canceled, wantTools: 2, wantProviders: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
				defer cancel()
				stop := func() {
					if tc.deadline {
						time.Sleep(time.Hour) // synctest advances time without a real wait.
						<-ctx.Done()
					} else {
						cancel()
					}
				}
				if tc.cancelAt == "before request" {
					stop()
				}
				var toolCalls atomic.Int32
				testTool := functool.MustNew(functool.Config{Name: "test_tool"}, func(context.Context, struct{}) (string, error) {
					n := toolCalls.Add(1)
					if tc.cancelAt == "first tool" && n == 1 || tc.cancelAt == "last tool" && n == 2 {
						stop()
					}
					if tc.cancelAt == "concurrent tools" {
						if n == 2 {
							stop()
						}
						<-ctx.Done() // Both invocations must start before cancellation.
					}
					return "result", tc.toolError
				})
				providerCalls := 0
				provider := func(ctx context.Context, _ []*message.Message, _ ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
					return func(yield func(*agent.ResponseUpdate, error) bool) {
						providerCalls++
						if providerCalls > 1 {
							if err := ctx.Err(); err != nil {
								yield(nil, err)
							} else {
								yield(&agent.ResponseUpdate{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "Done"}}}, nil)
							}
							return
						}
						if tc.cancelAt == "before tools" {
							stop()
						}
						yield(&agent.ResponseUpdate{Role: message.RoleAssistant, Contents: message.Contents{
							&message.FunctionCallContent{CallID: "1", Name: "test_tool", Arguments: `{}`},
							&message.FunctionCallContent{CallID: "2", Name: "test_tool", Arguments: `{}`},
						}}, nil)
					}
				}
				var runErr error
				for update, err := range toolautocall.New(toolautocall.Config{AllowConcurrentInvocations: tc.concurrent}).Run(
					provider, ctx, []*message.Message{message.NewText("Call both tools.")}, agent.WithTool(testTool)) {
					if err != nil {
						runErr = err
						break
					}
					if tc.cancelAt == "after results" && update != nil && update.Role == message.RoleTool {
						stop()
					}
				}
				var wantErr error
				if tc.cancelAt != "" {
					wantErr = context.Canceled
					if tc.deadline {
						wantErr = context.DeadlineExceeded
					}
				}
				if !errors.Is(runErr, wantErr) {
					t.Errorf("run error = %v, want %v", runErr, wantErr)
				}
				if got := toolCalls.Load(); got != tc.wantTools {
					t.Errorf("tool calls = %d, want %d", got, tc.wantTools)
				}
				if providerCalls != tc.wantProviders {
					t.Errorf("provider calls = %d, want %d", providerCalls, tc.wantProviders)
				}
			})
		})
	}
}
