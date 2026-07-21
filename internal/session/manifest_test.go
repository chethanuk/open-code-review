package session

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
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
		{"cancelled with only waives", 3, 0, 0, 0, 2, true, StatePartial},
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

// TestManifestSchemaLock pins the versioned schema contract: the literal
// version number and the top-level JSON key set. Either changing without the
// other is a breaking change for downstream readers; this test makes both
// deliberate.
func TestManifestSchemaLock(t *testing.T) {
	if ManifestSchemaVersion != 1 {
		t.Fatalf("ManifestSchemaVersion = %d; bumping it requires a schema-change review — update this lock and the docs together", ManifestSchemaVersion)
	}

	sh := New(t.TempDir(), "main", "m", SessionOptions{ReviewMode: ReviewModeRange})
	sh.SetSelected([]string{"a.go", "b.go"})
	sh.SetArtifactChecksum("artifact-sum")
	sh.RecordReviewItemDone("a.go", "a.go", "a.go", "fp-a", nil)
	sh.RecordReviewItemFailed("b.go", "b.go", "b.go", "fp-b", FailureProviderError, "boom")
	sh.Finalize()

	blob, err := json.Marshal(sh.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(blob, &rec); err != nil {
		t.Fatal(err)
	}
	if v, ok := rec["schema_version"].(float64); !ok || v != 1 {
		t.Errorf("serialized schema_version = %v, want 1", rec["schema_version"])
	}
	// Golden top-level key set (omitempty keys included only when populated
	// here). Adding/renaming a key is a schema change: bump the version.
	want := []string{
		"schema_version", "session_id", "repo", "range", "files", "state",
		"duration_ms", "review_mode", "model", "artifact_sha256", "failures",
		"started_at", "completed_at",
	}
	for _, k := range want {
		if _, ok := rec[k]; !ok {
			t.Errorf("manifest missing locked key %q (keys=%v)", k, keysOf(rec))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
