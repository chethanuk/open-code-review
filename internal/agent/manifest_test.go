package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/open-code-review/open-code-review/internal/config/template"
	"github.com/open-code-review/open-code-review/internal/llm"
	"github.com/open-code-review/open-code-review/internal/model"
	"github.com/open-code-review/open-code-review/internal/session"
	"github.com/open-code-review/open-code-review/internal/tool"
)

// routeClient dispatches on marker strings embedded in the rendered prompt so
// a single stateless client can drive success / fail / timeout / cancel /
// panic per file within one concurrent batch.
type routeClient struct{}

func (routeClient) CompletionsWithCtx(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.ExtractText())
	}
	text := sb.String()
	switch {
	case strings.Contains(text, "DO_PANIC"):
		panic("boom in subtask")
	case strings.Contains(text, "DO_TIMEOUT"):
		return nil, fmt.Errorf("llm call: %w", context.DeadlineExceeded)
	case strings.Contains(text, "DO_CANCEL"):
		return nil, fmt.Errorf("llm call: %w", context.Canceled)
	case strings.Contains(text, "DO_FAIL"):
		return nil, errors.New("provider exploded")
	default:
		content := ""
		return &llm.ChatResponse{
			Choices: []llm.Choice{{Message: llm.ResponseMessage{
				Content: &content,
				ToolCalls: []llm.ToolCall{{
					ID: "c1", Type: "function",
					Function: llm.FunctionCall{Name: "task_done", Arguments: "{}"},
				}},
			}}},
			Model: "fake",
			Usage: &llm.UsageInfo{PromptTokens: 5, CompletionTokens: 2},
		}, nil
	}
}

func classCounts(m *session.RunManifest) map[string]int {
	out := map[string]int{}
	for _, f := range m.Failures {
		out[f.Class]++
	}
	return out
}

func TestRunManifest_Scenarios(t *testing.T) {
	bigTpl := strings.Repeat("word ", 120) + " {{diff}}"

	tests := []struct {
		name          string
		maxTokens     int
		mainContent   string
		diffs         []model.Diff
		markCancelled bool
		wantState     string
		wantSelected  int
		wantClasses   map[string]int
	}{
		{
			name:         "success",
			maxTokens:    100000,
			mainContent:  "review {{diff}}",
			diffs:        []model.Diff{{NewPath: "ok.go", OldPath: "ok.go", Diff: "+ok", Insertions: 1}},
			wantState:    session.StateComplete,
			wantSelected: 1,
			wantClasses:  map[string]int{},
		},
		{
			name:        "mixed success and provider failure",
			maxTokens:   100000,
			mainContent: "review {{diff}}",
			diffs: []model.Diff{
				{NewPath: "ok.go", OldPath: "ok.go", Diff: "+ok", Insertions: 1},
				{NewPath: "bad.go", OldPath: "bad.go", Diff: "+DO_FAIL", Insertions: 1},
			},
			wantState:    session.StatePartial,
			wantSelected: 2,
			wantClasses:  map[string]int{session.FailureProviderError: 1},
		},
		{
			name:         "timeout classified",
			maxTokens:    100000,
			mainContent:  "review {{diff}}",
			diffs:        []model.Diff{{NewPath: "slow.go", OldPath: "slow.go", Diff: "+DO_TIMEOUT", Insertions: 1}},
			wantState:    session.StateFailed,
			wantSelected: 1,
			wantClasses:  map[string]int{session.FailureTimeout: 1},
		},
		{
			name:        "cancel with a success is partial",
			maxTokens:   100000,
			mainContent: "review {{diff}}",
			diffs: []model.Diff{
				{NewPath: "ok.go", OldPath: "ok.go", Diff: "+ok", Insertions: 1},
				{NewPath: "gone.go", OldPath: "gone.go", Diff: "+DO_CANCEL", Insertions: 1},
			},
			markCancelled: true,
			wantState:     session.StatePartial,
			wantSelected:  2,
			wantClasses:   map[string]int{session.FailureCancelled: 1},
		},
		{
			name:         "panic isolated and classified",
			maxTokens:    100000,
			mainContent:  "review {{diff}}",
			diffs:        []model.Diff{{NewPath: "panic.go", OldPath: "panic.go", Diff: "+DO_PANIC", Insertions: 1}},
			wantState:    session.StateFailed,
			wantSelected: 1,
			wantClasses:  map[string]int{session.FailurePanic: 1},
		},
		{
			name:         "skipped by token limit",
			maxTokens:    50,
			mainContent:  bigTpl,
			diffs:        []model.Diff{{NewPath: "huge.go", OldPath: "huge.go", Diff: "+x", Insertions: 1}},
			wantState:    session.StateFailed,
			wantSelected: 1,
			wantClasses:  map[string]int{session.FailureSkippedLimit: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := session.New(t.TempDir(), "main", "test", session.SessionOptions{ReviewMode: session.ReviewModeRange})
			a := New(Args{
				LLMClient: routeClient{},
				Model:     "test",
				Session:   sess,
				Tools:     tool.NewRegistry(),
				Template: template.Template{
					MaxTokens:           tt.maxTokens,
					MaxToolRequestTimes: 5,
					MainTask:            template.LlmConversation{Messages: []template.ChatMessage{{Role: "user", Content: tt.mainContent}}},
				},
				MainToolDefs: []llm.ToolDef{{Type: "function", Function: llm.FunctionDef{Name: "task_done", Description: "done"}}},
			})
			a.currentDate = "2026-07-15 08:00"
			a.diffs = tt.diffs

			_, _ = a.dispatchSubtasks(context.Background())
			if tt.markCancelled {
				sess.MarkCancelled()
			}
			sess.Finalize()

			m := sess.Manifest()
			if m == nil {
				t.Fatal("nil manifest")
			}
			if m.State != tt.wantState {
				t.Errorf("state = %q, want %q (files=%+v)", m.State, tt.wantState, m.Files)
			}
			if len(m.Files.Selected) != tt.wantSelected {
				t.Errorf("selected = %v, want %d", m.Files.Selected, tt.wantSelected)
			}
			if m.ArtifactSHA256 == "" {
				t.Error("artifact checksum should be recorded")
			}
			got := classCounts(m)
			if len(got) != len(tt.wantClasses) {
				t.Errorf("failure classes = %v, want %v", got, tt.wantClasses)
			}
			for k, v := range tt.wantClasses {
				if got[k] != v {
					t.Errorf("class %q count = %d, want %d (all=%v)", k, got[k], v, got)
				}
			}
		})
	}
}

// TestWaiveCoverage verifies a waived diff is not dispatched, is recorded as
// covered, and flips an otherwise-partial run to complete.
func TestWaiveCoverage(t *testing.T) {
	build := func(waive []string) *session.RunManifest {
		sess := session.New(t.TempDir(), "main", "test", session.SessionOptions{ReviewMode: session.ReviewModeRange})
		a := New(Args{
			LLMClient: routeClient{},
			Model:     "test",
			Session:   sess,
			Tools:     tool.NewRegistry(),
			Template: template.Template{
				MaxTokens:           100000,
				MaxToolRequestTimes: 5,
				MainTask:            template.LlmConversation{Messages: []template.ChatMessage{{Role: "user", Content: "{{diff}}"}}},
			},
			MainToolDefs: []llm.ToolDef{{Type: "function", Function: llm.FunctionDef{Name: "task_done"}}},
			// Resume must be non-nil for applyResume (and thus waive) to run.
			Resume:     &session.ResumeState{SessionID: "prev", Items: map[string]session.ResumeItem{}},
			WaivePaths: waive,
		})
		a.currentDate = "d"
		a.diffs = []model.Diff{
			{NewPath: "ok.go", OldPath: "ok.go", Diff: "+ok", Insertions: 1},
			{NewPath: "flaky.go", OldPath: "flaky.go", Diff: "+DO_FAIL", Insertions: 1},
		}
		_, _ = a.dispatchSubtasks(context.Background())
		sess.Finalize()
		return sess.Manifest()
	}

	// No waive: the failing diff leaves the run partial.
	noWaive := build(nil)
	if noWaive.State != session.StatePartial {
		t.Errorf("without waive: state = %q, want partial (files=%+v)", noWaive.State, noWaive.Files)
	}

	// Waiving the failing diff flips the run to complete and records it as
	// waived rather than dispatched/failed.
	waived := build([]string{"flaky.go"})
	if waived.State != session.StateComplete {
		t.Errorf("with waive: state = %q, want complete (files=%+v)", waived.State, waived.Files)
	}
	if len(waived.Files.Waived) != 1 || waived.Files.Waived[0] != "flaky.go" {
		t.Errorf("waived set = %v, want [flaky.go]", waived.Files.Waived)
	}
	if len(waived.Files.Failed) != 0 {
		t.Errorf("waived diff must not be recorded as failed: %v", waived.Files.Failed)
	}
	if len(waived.Files.Completed) != 1 || waived.Files.Completed[0] != "ok.go" {
		t.Errorf("completed set = %v, want [ok.go]", waived.Files.Completed)
	}
}

// TestArtifactChecksumStableAndOrderIndependent verifies the artifact hash is
// deterministic regardless of diff ordering (fingerprints are sorted).
func TestArtifactChecksumStableAndOrderIndependent(t *testing.T) {
	mk := func(order []model.Diff) string {
		sess := session.New(t.TempDir(), "main", "test", session.SessionOptions{ReviewMode: session.ReviewModeRange})
		a := New(Args{
			LLMClient: routeClient{},
			Model:     "test",
			Session:   sess,
			Tools:     tool.NewRegistry(),
			Template: template.Template{
				MaxTokens:           100000,
				MaxToolRequestTimes: 5,
				MainTask:            template.LlmConversation{Messages: []template.ChatMessage{{Role: "user", Content: "{{diff}}"}}},
			},
			MainToolDefs: []llm.ToolDef{{Type: "function", Function: llm.FunctionDef{Name: "task_done"}}},
		})
		a.currentDate = "d"
		a.diffs = order
		a.recordSelectionAndArtifact()
		sess.Finalize()
		return sess.Manifest().ArtifactSHA256
	}
	d1 := model.Diff{NewPath: "a.go", OldPath: "a.go", Diff: "+a", Insertions: 1}
	d2 := model.Diff{NewPath: "b.go", OldPath: "b.go", Diff: "+b", Insertions: 1}
	forward := mk([]model.Diff{d1, d2})
	reverse := mk([]model.Diff{d2, d1})
	if forward != reverse {
		t.Errorf("artifact checksum order-dependent: %q vs %q", forward, reverse)
	}
	if forward == "" {
		t.Error("empty checksum")
	}
}
