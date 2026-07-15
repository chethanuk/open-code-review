package session

import (
	"context"
	"errors"
	"sort"
)

// Failure classes recorded per failed review item. ClassifyFailure maps a Go
// error onto one of these; panic and skipped_limit are set explicitly by the
// dispatch paths that detect them.
const (
	FailureProviderError = "provider_error"
	FailureTimeout       = "timeout"
	FailureCancelled     = "cancelled"
	FailurePanic         = "panic"
	FailureSkippedLimit  = "skipped_limit"
)

// ClassifyFailure maps an error to a failure class using the standard context
// error chain. Anything that is not a timeout/cancellation is treated as a
// provider error (the common case: LLM/API failure).
func ClassifyFailure(err error) string {
	switch {
	case err == nil:
		return FailureProviderError
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.Is(err, context.Canceled):
		return FailureCancelled
	default:
		return FailureProviderError
	}
}

// ManifestSchemaVersion is the current run_manifest schema version. Bump it
// only on a breaking change to the field set so downstream readers can gate.
const ManifestSchemaVersion = 1

// Terminal run states. These are the machine contract surfaced in the run
// manifest and in `--format json`; they are independent of the legacy JSON
// `status` field and of the process exit code (which stays 0 for partial).
const (
	StateComplete = "complete"
	StatePartial  = "partial"
	StateFailed   = "failed"
	StateSkipped  = "skipped"
)

// ManifestRepo identifies the repository under review.
type ManifestRepo struct {
	RemoteURL string `json:"remote_url,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Dir       string `json:"dir,omitempty"`
}

// ManifestRange records the review range and its resolved commit SHAs.
type ManifestRange struct {
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Commit    string `json:"commit,omitempty"`
	FromSHA   string `json:"from_sha,omitempty"`
	ToSHA     string `json:"to_sha,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
}

// ManifestFiles holds the per-outcome coverage sets, each a sorted, de-duped
// list of file paths. selected == completed + reused + failed + waived is the
// coverage invariant the manifest asserts.
type ManifestFiles struct {
	Selected  []string `json:"selected"`
	Completed []string `json:"completed"`
	Reused    []string `json:"reused"`
	Failed    []string `json:"failed"`
	Waived    []string `json:"waived"`
}

// ManifestFailure is one failed review item with its typed class.
type ManifestFailure struct {
	Path  string `json:"path"`
	Class string `json:"class"`
	Error string `json:"error,omitempty"`
}

// RunManifest is the versioned, immutable summary of one review/scan run. It
// is written exactly once as the last line of the session JSONL file and is
// surfaced verbatim in `--format json` — the same Go value serialized both
// places, so persisted and reported coverage are identical by construction.
type RunManifest struct {
	SchemaVersion   int               `json:"schema_version"`
	SessionID       string            `json:"session_id"`
	ParentSessionID string            `json:"parent_session_id,omitempty"`
	Repo            ManifestRepo      `json:"repo"`
	ReviewMode      string            `json:"review_mode,omitempty"`
	Range           ManifestRange     `json:"range"`
	OCRVersion      string            `json:"ocr_version,omitempty"`
	Provider        string            `json:"provider,omitempty"`
	Model           string            `json:"model,omitempty"`
	Concurrency     int               `json:"concurrency,omitempty"`
	ConfigHash      string            `json:"config_hash,omitempty"`
	RulesHash       string            `json:"rules_hash,omitempty"`
	ArtifactSHA256  string            `json:"artifact_sha256,omitempty"`
	Files           ManifestFiles     `json:"files"`
	Failures        []ManifestFailure `json:"failures,omitempty"`
	State           string            `json:"state"`
	StartedAt       string            `json:"started_at,omitempty"`
	CompletedAt     string            `json:"completed_at,omitempty"`
	DurationMS      int64             `json:"duration_ms"`
}

// ComputeTerminalState derives the run's terminal state from the coverage
// counts. It is pure so it can be table-tested exhaustively.
//
// Rules (checked in order):
//   - nothing selected               -> skipped
//   - cancelled                      -> partial if any item succeeded, else failed
//   - every selected item failed     -> failed
//   - completed+reused+waived covers selected with no failures -> complete
//     (a waive satisfies the coverage contract, same as a completed review)
//   - anything else (mixed outcomes) -> partial
func ComputeTerminalState(selected, completed, reused, failed, waived int, cancelled bool) string {
	if selected == 0 {
		return StateSkipped
	}
	if cancelled {
		if completed+reused > 0 {
			return StatePartial
		}
		return StateFailed
	}
	if failed == selected {
		return StateFailed
	}
	if failed == 0 && completed+reused+waived >= selected {
		return StateComplete
	}
	return StatePartial
}

// sortedKeys returns the map keys as a sorted slice. Always non-nil so the
// manifest emits [] rather than null for empty sets.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
