package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestManifestAcceptanceMatrix walks the terminal-state scenarios end to end
// through the SessionHistory API (the same calls the agent/scan dispatch paths
// make) and asserts the persisted + in-memory manifest agree with the coverage.
func TestManifestAcceptanceMatrix(t *testing.T) {
	type step struct {
		kind string // done | reused | failed | waived
		path string
	}
	tests := []struct {
		name        string
		selected    []string
		steps       []step
		cancelled   bool
		wantState   string
		wantClasses map[string]int
	}{
		{
			name:      "clean complete",
			selected:  []string{"a.go", "b.go"},
			steps:     []step{{"done", "a.go"}, {"done", "b.go"}},
			wantState: StateComplete,
		},
		{
			name:        "partial with a reuse and a failure",
			selected:    []string{"a.go", "b.go", "c.go"},
			steps:       []step{{"done", "a.go"}, {"reused", "b.go"}, {"failed", "c.go"}},
			wantState:   StatePartial,
			wantClasses: map[string]int{FailureProviderError: 1},
		},
		{
			name:      "all failed",
			selected:  []string{"a.go"},
			steps:     []step{{"failed", "a.go"}},
			wantState: StateFailed,
		},
		{
			name:      "skipped when nothing selected",
			selected:  nil,
			steps:     nil,
			wantState: StateSkipped,
		},
		{
			name:      "cancelled but one succeeded is partial",
			selected:  []string{"a.go", "b.go"},
			steps:     []step{{"done", "a.go"}, {"failed", "b.go"}},
			cancelled: true,
			wantState: StatePartial,
		},
		{
			// selected is a SUPERSET here: b.go and c.go were never dispatched,
			// so they land in no outcome bucket — exactly the truncated-run
			// shape the partial state exists for.
			name:      "cancelled mid-run leaves uncovered files",
			selected:  []string{"a.go", "b.go", "c.go"},
			steps:     []step{{"done", "a.go"}},
			cancelled: true,
			wantState: StatePartial,
		},
		{
			name:      "waive completes an otherwise-partial run",
			selected:  []string{"a.go", "b.go"},
			steps:     []step{{"done", "a.go"}, {"waived", "b.go"}},
			wantState: StateComplete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sh := New(t.TempDir(), "main", "m", SessionOptions{ReviewMode: ReviewModeRange})
			sh.SetSelected(tt.selected)
			sh.SetArtifactChecksum("artifact-sum")
			for _, s := range tt.steps {
				switch s.kind {
				case "done":
					sh.RecordReviewItemDone(s.path, s.path, s.path, "fp-"+s.path, nil)
				case "reused":
					sh.RecordReviewItemReused(s.path, s.path, s.path, "fp-"+s.path, "prev", nil)
				case "failed":
					sh.RecordReviewItemFailed(s.path, s.path, s.path, "fp-"+s.path, FailureProviderError, "boom")
				case "waived":
					sh.RecordReviewItemWaived(s.path, s.path, s.path, "fp-"+s.path)
				}
			}
			if tt.cancelled {
				sh.MarkCancelled()
			}
			sh.Finalize()

			m := sh.Manifest()
			if m == nil {
				t.Fatal("nil manifest")
			}
			if m.State != tt.wantState {
				t.Errorf("state = %q, want %q (files=%+v)", m.State, tt.wantState, m.Files)
			}
			// Coverage invariant: selected is a superset of the outcome sets —
			// every recorded step lands in exactly one bucket, and selected
			// keeps the full denominator even when steps don't cover it.
			sum := len(m.Files.Completed) + len(m.Files.Reused) + len(m.Files.Failed) + len(m.Files.Waived)
			if sum != len(tt.steps) {
				t.Errorf("outcome buckets hold %d paths, want %d (one per step): %+v", sum, len(tt.steps), m.Files)
			}
			if len(m.Files.Selected) != len(tt.selected) {
				t.Errorf("selected = %v, want the full denominator %v", m.Files.Selected, tt.selected)
			}
			if len(tt.selected) > 0 && m.ArtifactSHA256 != "artifact-sum" {
				t.Errorf("artifact_sha256 = %q, want artifact-sum", m.ArtifactSHA256)
			}
			gotClasses := map[string]int{}
			for _, f := range m.Failures {
				gotClasses[f.Class]++
			}
			for k, v := range tt.wantClasses {
				if gotClasses[k] != v {
					t.Errorf("failure class %q = %d, want %d (all=%v)", k, gotClasses[k], v, gotClasses)
				}
			}

			// Persisted last record matches the in-memory state.
			path, err := SessionFilePath(sh.RepoDir, sh.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			rec := lastJSONLRecord(t, path)
			if rec["type"] != "run_manifest" || rec["state"] != tt.wantState {
				t.Errorf("persisted manifest mismatch: type=%v state=%v want %q", rec["type"], rec["state"], tt.wantState)
			}
		})
	}
}

// TestProviderTransitionOnResume covers the issue-367 resume contract in one
// flow: a mixed-success run terminates partial; a resume (here also switching
// provider + model) reuses the succeeded item, re-reviews the failed one, and
// flips the run to complete — recording the new provider/model, linking
// parent_session_id, and preserving the input identity (same artifact
// checksum, since the inputs are unchanged).
func TestProviderTransitionOnResume(t *testing.T) {
	repo := t.TempDir()
	artifact := ArtifactChecksum([]string{"fp-a", "fp-b"})

	// First run: anthropic; a.go succeeds, b.go fails -> partial.
	s1 := New(repo, "main", "claude-A", SessionOptions{
		ReviewMode: ReviewModeRange,
		RunMeta:    RunMeta{Provider: "anthropic", OCRVersion: "1.0.0"},
	})
	s1.SetSelected([]string{"a.go", "b.go"})
	s1.SetArtifactChecksum(artifact)
	s1.RecordReviewItemDone("a.go", "a.go", "a.go", "fp-a", nil)
	s1.RecordReviewItemFailed("b.go", "b.go", "b.go", "fp-b", FailureProviderError, "boom")
	s1.Finalize()
	if got := s1.Manifest().State; got != StatePartial {
		t.Fatalf("first run state = %q, want partial", got)
	}

	// Second run: resume from s1, now on openai with a different model. a.go is
	// reused from the prior run; the failed b.go is re-reviewed successfully.
	s2 := New(repo, "main", "gpt-B", SessionOptions{
		ReviewMode:  ReviewModeRange,
		ResumedFrom: s1.SessionID,
		RunMeta:     RunMeta{Provider: "openai", OCRVersion: "1.0.0"},
	})
	s2.SetSelected([]string{"a.go", "b.go"})
	s2.SetArtifactChecksum(artifact)
	s2.RecordReviewItemReused("a.go", "a.go", "a.go", "fp-a", s1.SessionID, nil)
	s2.RecordReviewItemDone("b.go", "b.go", "b.go", "fp-b", nil)
	s2.Finalize()

	m := s2.Manifest()
	if m.Provider != "openai" {
		t.Errorf("provider = %q, want openai (transition recorded)", m.Provider)
	}
	if m.Model != "gpt-B" {
		t.Errorf("model = %q, want gpt-B", m.Model)
	}
	if m.ParentSessionID != s1.SessionID {
		t.Errorf("parent_session_id = %q, want %q", m.ParentSessionID, s1.SessionID)
	}
	if len(m.Files.Reused) != 1 || m.Files.Reused[0] != "a.go" {
		t.Errorf("reused = %v, want [a.go] (reused items intact)", m.Files.Reused)
	}
	if m.State != StateComplete {
		t.Errorf("state = %q, want complete (resume covered the failed item)", m.State)
	}
	if m.ArtifactSHA256 != s1.Manifest().ArtifactSHA256 {
		t.Errorf("artifact checksum changed across resume: %q vs %q (input identity must be retained)",
			m.ArtifactSHA256, s1.Manifest().ArtifactSHA256)
	}
}

// TestManifestNeverLeaksSecrets is the load-bearing redaction guard: a fully
// populated manifest, serialized, must contain none of the token/auth-header
// patterns. The RunManifest/RunMeta structs carry no secret-bearing field, so
// this locks that shape in place.
func TestManifestNeverLeaksSecrets(t *testing.T) {
	sh := New(t.TempDir(), "main", "claude-opus", SessionOptions{
		ReviewMode: ReviewModeRange,
		DiffFrom:   "base",
		DiffTo:     "head",
		RunMeta: RunMeta{
			OCRVersion:    "1.2.3",
			Provider:      "anthropic",
			Concurrency:   4,
			ConfigHash:    "deadbeef",
			RulesHash:     "cafebabe",
			RepoRemoteURL: "https://example.com/acme/repo.git",
			RepoHeadSHA:   "0123456789abcdef0123456789abcdef01234567",
			RangeFromSHA:  "aaaa",
			RangeToSHA:    "bbbb",
		},
	})
	sh.SetSelected([]string{"a.go"})
	sh.SetArtifactChecksum("artifact-sum")
	sh.RecordReviewItemDone("a.go", "a.go", "a.go", "fp-a", nil)
	sh.Finalize()

	blob, err := json.Marshal(sh.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(blob))
	for _, bad := range []string{"api_key", "auth_token", "authorization", "x-api-key", "bearer", "sk-ant", "supersecret", "password", "token\"", ":token@"} {
		if strings.Contains(lower, strings.ToLower(bad)) {
			t.Errorf("manifest leaks sensitive pattern %q: %s", bad, blob)
		}
	}
}
