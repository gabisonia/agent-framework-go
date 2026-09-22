// Copyright (c) Microsoft. All rights reserved.

package copilotprovider

import (
	"bytes"
	"context"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

func TestCopilotTool_FunctionInvocationIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		callID string
		trace  bool
	}{
		{name: "trace context", callID: "call-1", trace: true},
		{name: "nil trace context", callID: "call-2"},
		{name: "missing call ID", trace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type contextKey struct{}
			var traceContext context.Context
			if tc.trace {
				traceContext = context.WithValue(t.Context(), contextKey{}, "trace")
			}
			var toolCalls, middlewareCalls int
			fn := functool.MustNew(functool.Config{Name: "lookup", Description: "Look up a value"}, func(ctx context.Context, _ struct{}) (string, error) {
				toolCalls++
				if tc.trace && ctx.Value(contextKey{}) != "trace" {
					t.Error("tool lost the trace context")
				}
				return "found", nil
			})
			middleware := agent.FunctionInvocationMiddleware(func(next func(context.Context, *agent.FunctionInvocationContext) (any, error), ctx context.Context, invocation *agent.FunctionInvocationContext) (any, error) {
				middlewareCalls++
				if invocation.Function != fn || invocation.CallID != tc.callID {
					t.Errorf("unexpected invocation: %#v", invocation)
				}
				return next(ctx, invocation)
			})
			run := func(_ context.Context, _ []*message.Message, options ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
				return func(yield func(*agent.ResponseUpdate, error) bool) {
					wrapped, _ := agent.GetOption(options, agent.WithTool)
					converted, err := toCopilotTool(wrapped.(tool.FuncTool))
					if err != nil {
						yield(nil, err)
						return
					}
					_, err = converted.Handler(copilot.ToolInvocation{ToolCallID: tc.callID, Arguments: map[string]any{}, TraceContext: traceContext})
					if err != nil {
						yield(nil, err)
					}
				}
			}
			a := agent.New(agent.ProviderConfig{Run: run, Middlewares: []agent.Middleware{middleware}}, agent.Config{Tools: []tool.Tool{fn}})
			if _, err := a.RunText(t.Context(), "lookup").Collect(); err != nil {
				t.Fatal(err)
			}
			if middlewareCalls != 1 || toolCalls != 1 {
				t.Errorf("calls = middleware:%d tool:%d, want both 1", middlewareCalls, toolCalls)
			}
		})
	}
}

func TestSessionConfig_WithApprovalRequiredTool_InstallsAskPreToolUseHook(t *testing.T) {
	dangerousTool := tool.ApprovalRequiredFunc(testFuncTool(t, "dangerous"))
	plainTool := testFuncTool(t, "plain")
	p := &provider{}

	cfg := p.sessionConfig(true, nil, []agent.Option{
		agent.WithTool(dangerousTool),
		agent.WithTool(plainTool),
	})

	if cfg.Hooks == nil || cfg.Hooks.OnPreToolUse == nil {
		t.Fatal("OnPreToolUse hook was not installed")
	}

	dangerousDecision, err := cfg.Hooks.OnPreToolUse(copilot.PreToolUseHookInput{ToolName: "dangerous"}, copilot.HookInvocation{})
	if err != nil {
		t.Fatalf("OnPreToolUse(dangerous): %v", err)
	}
	if dangerousDecision == nil || dangerousDecision.PermissionDecision != "ask" {
		t.Fatalf("dangerous permission decision = %#v, want ask", dangerousDecision)
	}

	plainDecision, err := cfg.Hooks.OnPreToolUse(copilot.PreToolUseHookInput{ToolName: "plain"}, copilot.HookInvocation{})
	if err != nil {
		t.Fatalf("OnPreToolUse(plain): %v", err)
	}
	if plainDecision != nil {
		t.Fatalf("plain permission decision = %#v, want nil", plainDecision)
	}
}

func TestSessionConfig_RawSessionConfigToolNotGatedButFrameworkApprovalToolIs(t *testing.T) {
	// A raw copilot.Tool supplied via SessionConfig.Tools carries no approval
	// marker, so it must not be auto-gated. Only tools explicitly marked
	// approval-required via tool.ApprovalRequiredFunc are gated, mirroring .NET's
	// ApprovalRequiredAIFunction (SkipPermission is a separate, orthogonal concept).
	source := &copilot.SessionConfig{
		Tools: []copilot.Tool{{Name: "dangerous"}},
		Hooks: &copilot.SessionHooks{},
	}
	p := &provider{cfg: AgentConfig{SessionConfig: source}}

	cfg := p.sessionConfig(true, nil, []agent.Option{
		agent.WithTool(tool.ApprovalRequiredFunc(testFuncTool(t, "fw-dangerous"))),
	})

	if source.Hooks.OnPreToolUse != nil {
		t.Fatal("source hooks were mutated")
	}
	if len(source.Tools) != 1 {
		t.Fatalf("source tools length = %d, want 1", len(source.Tools))
	}
	if len(cfg.Tools) != 2 {
		t.Fatalf("session tools length = %d, want 2", len(cfg.Tools))
	}
	if cfg.Hooks == nil || cfg.Hooks == source.Hooks {
		t.Fatal("session hooks were not cloned")
	}

	// The explicitly-marked framework tool is gated.
	fw, err := cfg.Hooks.OnPreToolUse(copilot.PreToolUseHookInput{ToolName: "fw-dangerous"}, copilot.HookInvocation{})
	if err != nil {
		t.Fatalf("OnPreToolUse(fw-dangerous): %v", err)
	}
	if fw == nil || fw.PermissionDecision != "ask" {
		t.Fatalf("fw-dangerous permission decision = %#v, want ask", fw)
	}

	// The raw SessionConfig tool carries no approval marker and must not be gated.
	raw, err := cfg.Hooks.OnPreToolUse(copilot.PreToolUseHookInput{ToolName: "dangerous"}, copilot.HookInvocation{})
	if err != nil {
		t.Fatalf("OnPreToolUse(dangerous): %v", err)
	}
	if raw != nil {
		t.Fatalf("raw session-config tool should not be gated, got %#v", raw)
	}
}

func TestSessionConfig_WithExistingPreToolUseHook_PreservesCallerHook(t *testing.T) {
	expected := &copilot.PreToolUseHookOutput{PermissionDecision: "allow"}
	source := &copilot.SessionConfig{
		Hooks: &copilot.SessionHooks{
			OnPreToolUse: func(copilot.PreToolUseHookInput, copilot.HookInvocation) (*copilot.PreToolUseHookOutput, error) {
				return expected, nil
			},
		},
	}
	p := &provider{cfg: AgentConfig{SessionConfig: source}}

	cfg := p.sessionConfig(true, nil, []agent.Option{agent.WithTool(tool.ApprovalRequiredFunc(testFuncTool(t, "dangerous")))})

	if cfg.Hooks == nil || cfg.Hooks.OnPreToolUse == nil {
		t.Fatal("OnPreToolUse hook is nil")
	}
	got, err := cfg.Hooks.OnPreToolUse(copilot.PreToolUseHookInput{ToolName: "dangerous"}, copilot.HookInvocation{})
	if err != nil {
		t.Fatalf("OnPreToolUse: %v", err)
	}
	if got != expected {
		t.Fatalf("permission decision = %#v, want %#v", got, expected)
	}
}

func TestSessionConfig_WithExistingPreToolUseHookAndApprovalTools_LogsWarning(t *testing.T) {
	var logs bytes.Buffer
	defaultLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(defaultLogger)
	})

	source := &copilot.SessionConfig{
		Hooks: &copilot.SessionHooks{
			OnPreToolUse: func(copilot.PreToolUseHookInput, copilot.HookInvocation) (*copilot.PreToolUseHookOutput, error) {
				return nil, nil
			},
		},
	}
	p := &provider{cfg: AgentConfig{SessionConfig: source}}

	_ = p.sessionConfig(true, nil, []agent.Option{
		agent.WithTool(tool.ApprovalRequiredFunc(testFuncTool(t, "dangerous-a"))),
		agent.WithTool(tool.ApprovalRequiredFunc(testFuncTool(t, "dangerous-b"))),
	})

	logOutput := logs.String()
	if !strings.Contains(logOutput, "not be automatically gated") {
		t.Fatalf("expected warning log for skipped approval gating, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "approvalRequiredToolCount=2") {
		t.Fatalf("expected tool count in warning log, got %q", logOutput)
	}
	if !strings.Contains(logOutput, "dangerous-a, dangerous-b") {
		t.Fatalf("expected tool names in warning log, got %q", logOutput)
	}
}

func TestResumeSessionConfig_WithApprovalRequiredTool_InstallsAskPreToolUseHook(t *testing.T) {
	p := &provider{}

	cfg := p.resumeSessionConfig(true, nil, []agent.Option{
		agent.WithTool(tool.ApprovalRequiredFunc(testFuncTool(t, "dangerous"))),
	})

	if cfg.Hooks == nil || cfg.Hooks.OnPreToolUse == nil {
		t.Fatal("OnPreToolUse hook was not installed")
	}
	decision, err := cfg.Hooks.OnPreToolUse(copilot.PreToolUseHookInput{ToolName: "dangerous"}, copilot.HookInvocation{})
	if err != nil {
		t.Fatalf("OnPreToolUse: %v", err)
	}
	if decision == nil || decision.PermissionDecision != "ask" {
		t.Fatalf("dangerous permission decision = %#v, want ask", decision)
	}
}

func TestCopyResumeSessionConfig_CopiesRecentSDKFields(t *testing.T) {
	githubMCPToolConfig := &copilot.GitHubMCPToolConfig{}
	managedSettings := &copilot.ManagedSettings{}
	source := &copilot.SessionConfig{
		AdditionalDirectories:    []string{"/shared"},
		EnableFileChangeTracking: new(true),
		EnableExperimentalMode:   new(true),
		DisabledMCPServers:       []string{"legacy"},
		GitHubMCPToolConfig:      githubMCPToolConfig,
		ManagedSettings:          managedSettings,
	}

	got := copyResumeSessionConfig(source)

	if !slices.Equal(got.AdditionalDirectories, source.AdditionalDirectories) {
		t.Errorf("AdditionalDirectories = %v, want %v", got.AdditionalDirectories, source.AdditionalDirectories)
	}
	if got.EnableFileChangeTracking != source.EnableFileChangeTracking {
		t.Error("EnableFileChangeTracking was not preserved")
	}
	if got.EnableExperimentalMode != source.EnableExperimentalMode {
		t.Error("EnableExperimentalMode was not preserved")
	}
	if !slices.Equal(got.DisabledMCPServers, source.DisabledMCPServers) {
		t.Errorf("DisabledMCPServers = %v, want %v", got.DisabledMCPServers, source.DisabledMCPServers)
	}
	if got.GitHubMCPToolConfig != githubMCPToolConfig {
		t.Error("GitHubMCPToolConfig was not preserved")
	}
	if got.ManagedSettings != managedSettings {
		t.Error("ManagedSettings was not preserved")
	}
}

func testFuncTool(t *testing.T, name string) tool.FuncTool {
	t.Helper()
	return functool.MustNew(
		functool.Config{Name: name, Description: name + " description"},
		func(context.Context, struct{}) (string, error) { return "ok", nil },
	)
}
