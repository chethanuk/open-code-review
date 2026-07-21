package scan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/open-code-review/open-code-review/internal/llm"
	"github.com/open-code-review/open-code-review/internal/model"
	"github.com/open-code-review/open-code-review/internal/session"
	"github.com/open-code-review/open-code-review/internal/tool"
)

// scanRouteClient dispatches on markers in the rendered prompt (which embeds
// {{file_content}}) so one client can drive success/fail/cancel per file.
type scanRouteClient struct{}

func (scanRouteClient) CompletionsWithCtx(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.ExtractText())
	}
	text := sb.String()
	switch {
	case strings.Contains(text, "DO_PANIC"):
		panic("scan client exploded")
	case strings.Contains(text, "DO_CANCEL"):
		return nil, fmt.Errorf("scan call: %w", context.Canceled)
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
			Usage: &llm.UsageInfo{PromptTokens: 5, CompletionTokens: 2},
		}, nil
	}
}

func scanClassCounts(m *session.RunManifest) map[string]int {
	out := map[string]int{}
	for _, f := range m.Failures {
		out[f.Class]++
	}
	return out
}

// The scan fingerprint must be content-sensitive: the same path with changed
// content is a different input, so the manifest's artifact checksum changes.
func TestScanFingerprintContentSensitive(t *testing.T) {
	a := scanFingerprint(model.ScanItem{Path: "a.go", Content: "v1"})
	b := scanFingerprint(model.ScanItem{Path: "a.go", Content: "v2"})
	if a == b {
		t.Error("fingerprint identical across different file contents")
	}
	if a != scanFingerprint(model.ScanItem{Path: "a.go", Content: "v1"}) {
		t.Error("fingerprint not stable for identical input")
	}
}

func TestScanRunManifest_Parity(t *testing.T) {
	tests := []struct {
		name          string
		maxTokens     int
		items         []model.ScanItem
		markCancelled bool
		wantState     string
		wantSelected  int
		wantClasses   map[string]int
	}{
		{
			name:         "success",
			maxTokens:    100000,
			items:        []model.ScanItem{{Path: "ok.go", Content: "ok", LineCount: 1}},
			wantState:    session.StateComplete,
			wantSelected: 1,
			wantClasses:  map[string]int{},
		},
		{
			name:      "mixed success and failure",
			maxTokens: 100000,
			items: []model.ScanItem{
				{Path: "ok.go", Content: "ok", LineCount: 1},
				{Path: "bad.go", Content: "DO_FAIL", LineCount: 1},
			},
			wantState:    session.StatePartial,
			wantSelected: 2,
			wantClasses:  map[string]int{session.FailureProviderError: 1},
		},
		{
			name:         "all failed",
			maxTokens:    100000,
			items:        []model.ScanItem{{Path: "bad.go", Content: "DO_FAIL", LineCount: 1}},
			wantState:    session.StateFailed,
			wantSelected: 1,
			wantClasses:  map[string]int{session.FailureProviderError: 1},
		},
		{
			name:      "cancel with a success is partial",
			maxTokens: 100000,
			items: []model.ScanItem{
				{Path: "ok.go", Content: "ok", LineCount: 1},
				{Path: "gone.go", Content: "DO_CANCEL", LineCount: 1},
			},
			markCancelled: true,
			wantState:     session.StatePartial,
			wantSelected:  2,
			wantClasses:   map[string]int{session.FailureCancelled: 1},
		},
		{
			name:      "panic isolated to one file",
			maxTokens: 100000,
			items: []model.ScanItem{
				{Path: "ok.go", Content: "ok", LineCount: 1},
				{Path: "boom.go", Content: "DO_PANIC", LineCount: 1},
			},
			wantState:    session.StatePartial,
			wantSelected: 2,
			wantClasses:  map[string]int{session.FailurePanic: 1},
		},
		{
			name:         "token limit skip",
			maxTokens:    10,
			items:        []model.ScanItem{{Path: "huge.go", Content: "some content here", LineCount: 1}},
			wantState:    session.StateFailed,
			wantSelected: 1,
			wantClasses:  map[string]int{session.FailureSkippedLimit: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tpl := makeTemplateWithFullScan()
			tpl.MaxTokens = tt.maxTokens
			sess := session.New(t.TempDir(), "", "test", session.SessionOptions{ReviewMode: session.ReviewModeFullScan})
			a := NewAgent(Args{
				Template:         tpl,
				LLMClient:        scanRouteClient{},
				Model:            "test",
				CommentCollector: tool.NewCommentCollector(),
				Tools:            tool.NewRegistry(),
				MaxConcurrency:   2,
				SkipPlan:         true,
				SkipDedup:        true,
				SkipSummary:      true,
				Session:          sess,
			})
			a.items = tt.items
			a.currentDate = "2026-07-15"
			a.args.Tools.Freeze()

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
			if m.ReviewMode != session.ReviewModeFullScan {
				t.Errorf("review_mode = %q, want full_scan", m.ReviewMode)
			}
			got := scanClassCounts(m)
			if len(got) != len(tt.wantClasses) {
				t.Errorf("failure classes = %v, want %v", got, tt.wantClasses)
			}
			for k, v := range tt.wantClasses {
				if got[k] != v {
					t.Errorf("class %q = %d, want %d (all=%v)", k, got[k], v, got)
				}
			}
		})
	}
}

// TestFilterLargeScansRecordsSkip verifies the content pre-filter records a
// dropped item as failed/skipped_limit so it stays in the manifest's coverage
// denominator instead of being silently deselected.
func TestFilterLargeScansRecordsSkip(t *testing.T) {
	tpl := makeTemplateWithFullScan()
	tpl.MaxTokens = 50
	sess := session.New(t.TempDir(), "", "test", session.SessionOptions{ReviewMode: session.ReviewModeFullScan})
	a := NewAgent(Args{
		Template:         tpl,
		LLMClient:        scanRouteClient{},
		Model:            "test",
		CommentCollector: tool.NewCommentCollector(),
		Tools:            tool.NewRegistry(),
		Session:          sess,
	})

	kept := a.filterLargeScans([]model.ScanItem{
		{Path: "ok.go", Content: "ok", LineCount: 1},
		{Path: "huge.go", Content: strings.Repeat("word ", 500), LineCount: 1},
	})
	if len(kept) != 1 || kept[0].Path != "ok.go" {
		t.Fatalf("kept = %+v, want only ok.go", kept)
	}

	sess.Finalize()
	m := sess.Manifest()
	if m == nil {
		t.Fatal("nil manifest")
	}
	if len(m.Files.Failed) != 1 || m.Files.Failed[0] != "huge.go" {
		t.Errorf("failed = %v, want [huge.go]", m.Files.Failed)
	}
	if got := scanClassCounts(m); got[session.FailureSkippedLimit] != 1 {
		t.Errorf("failure classes = %v, want skipped_limit=1", got)
	}
}
