package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

func TestBuildCodexArgsAddsRequiredFlags(t *testing.T) {
	args := buildCodexArgs([]string{"exec"}, "codex", "/tmp/work", "hello")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--json") {
		t.Fatalf("expected --json in args, got %v", args)
	}
	if !strings.Contains(joined, "--skip-git-repo-check") {
		t.Fatalf("expected --skip-git-repo-check in args, got %v", args)
	}
	if !strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("expected unattended automation flag in args, got %v", args)
	}
	if !strings.Contains(joined, "--cd /tmp/work") {
		t.Fatalf("expected working dir args, got %v", args)
	}
	if strings.Contains(joined, "--model codex") {
		t.Fatalf("did not expect generic codex model override, got %v", args)
	}
	if args[len(args)-1] != "hello" {
		t.Fatalf("expected prompt to be final arg, got %v", args)
	}
}

func TestConsumeCodexStdoutNormalizesEvents(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-1"}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'ls -1'","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"hello from codex"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":8}}`,
	}, "\n")

	state := &codexExecutionState{}
	events := make([]types.CommandStreamEvent, 0, 4)
	consumeCodexStdout(strings.NewReader(lines), state, func(event types.CommandStreamEvent) {
		events = append(events, event)
	})

	if state.threadID != "thread-1" {
		t.Fatalf("expected thread id to be captured, got %q", state.threadID)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 emitted events, got %d", len(events))
	}
	if events[0].Type != "tool_actions" || len(events[0].Actions) != 1 {
		t.Fatalf("expected first event to be tool_actions, got %+v", events[0])
	}
	if events[1].Type != "content" || events[1].Content != "hello from codex" {
		t.Fatalf("expected content event, got %+v", events[1])
	}
	if events[2].Type != "usage" || events[2].Usage == nil || events[2].Usage.TotalTokens != 20 {
		t.Fatalf("expected usage event with total tokens, got %+v", events[2])
	}
}

func TestRepositoryLookupCandidatesFallsBackToBasename(t *testing.T) {
	candidates := repositoryLookupCandidates("/home/agent/repos/mantler")
	if len(candidates) < 2 {
		t.Fatalf("expected multiple candidates, got %v", candidates)
	}
	if candidates[0] != "/home/agent/repos/mantler" {
		t.Fatalf("expected full path candidate first, got %v", candidates)
	}
	if candidates[1] != "mantler" {
		t.Fatalf("expected basename candidate second, got %v", candidates)
	}
}

func TestBuildGooseReplyRequestSplitsHistoryAndCurrentUserTurn(t *testing.T) {
	request := buildGooseReplyRequest("sess-1", []harnessExecMessage{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "First user turn"},
		{Role: "assistant", Content: "Prior assistant reply"},
		{Role: "user", Content: "Latest user turn"},
	})

	if got, _ := request["session_id"].(string); got != "sess-1" {
		t.Fatalf("expected session_id to be preserved, got %#v", request["session_id"])
	}
	userMessage, ok := request["user_message"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected user_message map, got %#v", request["user_message"])
	}
	if userMessage["role"] != "user" {
		t.Fatalf("expected current user_message role=user, got %#v", userMessage["role"])
	}
	override, ok := request["override_conversation"].([]map[string]interface{})
	if ok {
		if len(override) != 3 {
			t.Fatalf("expected 3 history messages, got %d", len(override))
		}
		return
	}
	overrideAny, ok := request["override_conversation"].([]interface{})
	if !ok || len(overrideAny) != 3 {
		t.Fatalf("expected 3 history messages, got %#v", request["override_conversation"])
	}
}

func TestConsumeGooseReplyStreamNormalizesEvents(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"Message","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]},"token_state":{"accumulatedInputTokens":8,"accumulatedOutputTokens":3,"accumulatedTotalTokens":11}}`,
		``,
		`data: {"type":"Message","message":{"role":"assistant","content":[{"type":"text","text":"hello world"},{"type":"toolRequest","id":"tool-1","toolCall":{"name":"shell"}}]},"token_state":{"accumulatedInputTokens":8,"accumulatedOutputTokens":7,"accumulatedTotalTokens":15}}`,
		``,
		`data: {"type":"Finish","reason":"done","token_state":{"accumulatedInputTokens":8,"accumulatedOutputTokens":7,"accumulatedTotalTokens":15}}`,
		``,
	}, "\n")

	state := &gooseExecutionState{}
	events := make([]types.CommandStreamEvent, 0, 4)
	err := consumeGooseReplyStream(strings.NewReader(stream), state, func(event types.CommandStreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("expected Goose stream to parse, got error: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 normalized events, got %d", len(events))
	}
	if events[0].Type != "content" || events[0].Content != "hello" {
		t.Fatalf("expected first content event, got %+v", events[0])
	}
	if events[1].Type != "content" || events[1].Content != " world" {
		t.Fatalf("expected delta content event, got %+v", events[1])
	}
	if events[2].Type != "tool_actions" || len(events[2].Actions) != 1 || events[2].Actions[0] != "tool: shell" {
		t.Fatalf("expected tool action event, got %+v", events[2])
	}
	if events[3].Type != "usage" || events[3].Usage == nil || events[3].Usage.TotalTokens != 15 {
		t.Fatalf("expected usage event with total tokens, got %+v", events[3])
	}
}

func TestRunCodexExecEmitsTaskSuccessOutcome(t *testing.T) {
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "fake-codex.sh")
	script := strings.Join([]string{
		"#!/bin/sh",
		"echo '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}'",
		"echo '{\"type\":\"item.completed\",\"item\":{\"id\":\"item_1\",\"type\":\"agent_message\",\"text\":\"done\"}}'",
		"echo '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}'",
		"exit 0",
	}, "\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	var outcomes []types.OutcomeEvent
	executor := &Executor{
		outcome: func(event types.OutcomeEvent) {
			outcomes = append(outcomes, event)
		},
	}

	_, err := executor.runCodexExec("cmd-1", harnessExecParams{
		HarnessID:           "h1",
		HarnessType:         "codex_cli",
		DirectTargetID:      "dt-1",
		TaskID:              "task-1",
		CompatibilityPlanID: "plan-1",
		MantleFingerprint:   "mf-1",
		BaseFingerprint:     "bf-1",
		TransportCommand:    scriptPath,
		TransportArgs:       []string{"exec"},
		Messages: []harnessExecMessage{
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if len(outcomes) == 0 {
		t.Fatalf("expected at least one outcome event")
	}
	last := outcomes[len(outcomes)-1]
	if last.EventType != "task_success" {
		t.Fatalf("expected task_success, got %s", last.EventType)
	}
	if last.TaskID != "task-1" || last.PlanID != "plan-1" || last.MantleFingerprint != "mf-1" || last.BaseFingerprint != "bf-1" {
		t.Fatalf("expected context fields to be propagated, got %+v", last)
	}
	if last.TokenUsage == nil || last.TokenUsage.PromptTokens != 3 || last.TokenUsage.CompletionTokens != 5 || last.TokenUsage.TotalTokens != 8 {
		t.Fatalf("expected token usage to be emitted, got %+v", last.TokenUsage)
	}
	if last.DurationMs < 0 {
		t.Fatalf("expected non-negative duration, got %d", last.DurationMs)
	}
	if strings.TrimSpace(last.Timestamp) == "" {
		t.Fatalf("expected timestamp to be set")
	}
	if _, err := time.Parse(time.RFC3339, last.Timestamp); err != nil {
		t.Fatalf("expected RFC3339 timestamp, got %q", last.Timestamp)
	}
}

func TestBuildGenericHarnessArgsTracksPromptInclusion(t *testing.T) {
	args, promptIncluded := buildGenericHarnessArgs("aider", []string{"--message", "{{prompt}}"}, "hello")
	if !promptIncluded {
		t.Fatalf("expected prompt placeholder to be detected")
	}
	if len(args) != 2 || args[1] != "hello" {
		t.Fatalf("expected prompt placeholder replacement, got %v", args)
	}
}

func TestBuildGenericHarnessArgsFallsBackToStdinWhenCustomArgsOmitPrompt(t *testing.T) {
	args, promptIncluded := buildGenericHarnessArgs("claude_code", []string{"--print"}, "hello")
	if promptIncluded {
		t.Fatalf("expected custom args without placeholder to require stdin fallback")
	}
	if len(args) != 1 || args[0] != "--print" {
		t.Fatalf("expected args to remain unchanged, got %v", args)
	}
}

func TestSupportsAgentHarnessExecutionIncludesExpandedCliSet(t *testing.T) {
	supported := []string{
		"codex_cli",
		"goose",
		"opencode",
		"aider",
		"claude_code",
		"open_interpreter",
		"openharness",
	}
	for _, harnessType := range supported {
		if !supportsAgentHarnessExecution(harnessType) {
			t.Fatalf("expected %s to be supported", harnessType)
		}
	}
}

func TestRunHarnessExecTreatsCustomOpenAIAsServerManaged(t *testing.T) {
	executor := &Executor{}
	result, err := executor.runHarnessExec(types.AgentCommand{
		ID:   "cmd-1",
		Type: "run_harness_exec",
		Params: map[string]interface{}{
			"harnessId":     "h-custom",
			"harnessType":   "custom_openai",
			"transportKind": "openai_http",
			"messages": []interface{}{
				map[string]interface{}{
					"role":    "user",
					"content": "hello",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("expected custom_openai openai_http to be handled, got error: %v", err)
	}
	if !strings.Contains(result.Details, "server-managed") {
		t.Fatalf("expected server-managed details, got %q", result.Details)
	}
}
