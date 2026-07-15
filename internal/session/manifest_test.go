package session

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestComputeTerminalState(t *testing.T) {
	tests := []struct {
		name                                        string
		selected, completed, reused, failed, waived int
		cancelled                                   bool
		want                                        string
	}{
		{"nothing selected", 0, 0, 0, 0, 0, false, StateSkipped},
		{"all completed", 3, 3, 0, 0, 0, false, StateComplete},
		{"all reused", 2, 0, 2, 0, 0, false, StateComplete},
		{"completed + reused covers", 4, 2, 2, 0, 0, false, StateComplete},
		{"waive completes the set", 3, 2, 0, 0, 1, false, StateComplete},
		{"all waived", 2, 0, 0, 0, 2, false, StateComplete},
		{"all failed", 3, 0, 0, 3, 0, false, StateFailed},
		{"mixed success and failure", 3, 2, 0, 1, 0, false, StatePartial},
		{"mixed with waive but a failure remains", 4, 1, 1, 1, 1, false, StatePartial},
		{"cancelled with a success", 5, 2, 0, 0, 0, true, StatePartial},
		{"cancelled with a reused success", 5, 0, 1, 0, 0, true, StatePartial},
		{"cancelled with nothing done", 5, 0, 0, 0, 0, true, StateFailed},
		{"cancelled with only failures", 5, 0, 0, 2, 0, true, StateFailed},
		{"cancelled overrides otherwise-complete", 2, 2, 0, 0, 0, true, StatePartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeTerminalState(tt.selected, tt.completed, tt.reused, tt.failed, tt.waived, tt.cancelled)
			if got != tt.want {
				t.Errorf("ComputeTerminalState(%d,%d,%d,%d,%d,%v) = %q, want %q",
					tt.selected, tt.completed, tt.reused, tt.failed, tt.waived, tt.cancelled, got, tt.want)
			}
		})
	}
}

func TestClassifyFailure(t *testing.T) {
	// context errors are covered indirectly; assert the plain mapping here.
	if got := ClassifyFailure(nil); got != FailureProviderError {
		t.Errorf("nil err = %q", got)
	}
}

// lastJSONLRecord returns the last non-empty JSONL record in a session file.
func lastJSONLRecord(t *testing.T, path string) map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var last map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		last = rec
	}
	return last
}

func TestFinalizeWritesRunManifest(t *testing.T) {
	sh := New(t.TempDir(), "main", "test-model", SessionOptions{
		ReviewMode: ReviewModeRange,
		DiffFrom:   "base",
		DiffTo:     "head",
		RunMeta: RunMeta{
			OCRVersion:  "1.2.3",
			Provider:    "anthropic",
			Concurrency: 4,
			ConfigHash:  "cfg-hash",
			RulesHash:   "rules-hash",
		},
	})
	sh.SetSelected([]string{"a.go", "b.go", "c.go"})
	sh.SetArtifactChecksum("artifact-sum")
	sh.RecordReviewItemDone("a.go", "a.go", "a.go", "fp-a", nil)
	sh.RecordReviewItemReused("b.go", "b.go", "b.go", "fp-b", "old-session", nil)
	sh.RecordReviewItemFailed("c.go", "c.go", "c.go", "fp-c", FailureProviderError, "boom")
	sh.Finalize()

	// In-memory manifest matches what we persisted.
	m := sh.Manifest()
	if m == nil {
		t.Fatal("Manifest() returned nil after Finalize")
	}
	if m.State != StatePartial {
		t.Errorf("state = %q, want partial", m.State)
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		t.Errorf("schema_version = %d", m.SchemaVersion)
	}
	if len(m.Files.Selected) != 3 || len(m.Files.Completed) != 1 || len(m.Files.Reused) != 1 || len(m.Files.Failed) != 1 {
		t.Errorf("coverage = %+v", m.Files)
	}
	if len(m.Failures) != 1 || m.Failures[0].Class != FailureProviderError {
		t.Errorf("failures = %+v", m.Failures)
	}

	// The LAST JSONL line is the run_manifest record with matching coverage.
	path, err := SessionFilePath(sh.RepoDir, sh.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	rec := lastJSONLRecord(t, path)
	if rec["type"] != "run_manifest" {
		t.Fatalf("last record type = %v, want run_manifest", rec["type"])
	}
	if rec["state"] != StatePartial {
		t.Errorf("persisted state = %v", rec["state"])
	}

	// No token/secret content leaks into the manifest record. The endpoint
	// token / auth header must never appear anywhere in the serialized line.
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"api_key", "auth_token", "sk-ant", "authorization", "x-api-key", "Bearer"} {
		if strings.Contains(strings.ToLower(string(blob)), strings.ToLower(bad)) {
			t.Errorf("manifest leaks sensitive token pattern %q: %s", bad, blob)
		}
	}
}
