// Package session provides a session history mechanism for collecting conversation
// records during code review task execution. It organizes records by file path
// and request type (plan_task, main_task, memory_compression_task).
package session

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-code-review/open-code-review/internal/llm"
	"github.com/open-code-review/open-code-review/internal/model"
)

// TaskType identifies the kind of LLM request within a file subtask.
type TaskType string

const (
	PlanTask              TaskType = "plan_task"
	MainTask              TaskType = "main_task"
	MemoryCompressionTask TaskType = "memory_compression_task"
	ReLocationTask        TaskType = "re_location_task"
	ReviewFilterTask      TaskType = "review_filter_task"
)

const (
	ReviewModeWorkspace = "workspace"
	ReviewModeRange     = "range"
	ReviewModeCommit    = "commit"
	ReviewModeFullScan  = "full_scan"
)

// SessionHistory is the top-level container for an entire CR run.
// It is safe for concurrent use by multiple goroutines.
type SessionHistory struct {
	mu           sync.Mutex
	SessionID    string
	RepoDir      string
	GitBranch    string
	Model        string
	ReviewMode   string
	DiffFrom     string
	DiffTo       string
	DiffCommit   string
	ResumedFrom  string
	StartTime    time.Time
	EndTime      time.Time
	persist      *jsonlWriter
	FileSessions map[string]*FileSession
	llmFailures  int64

	// runMeta carries the cmd-layer-populated manifest metadata (version,
	// provider, hashes, repo identity, resolved range SHAs). Static for the
	// life of the run.
	runMeta RunMeta

	// Coverage sets tracked for the run manifest. Guarded by mu. Each
	// terminal review item lands in exactly one of completed/reused/failed/
	// waived; selected is the full reviewable set. sortedKeys yields the
	// manifest's per-outcome path lists.
	selected    map[string]struct{}
	completed   map[string]struct{}
	reused      map[string]struct{}
	failed      map[string]struct{}
	waived      map[string]struct{}
	failures    []ManifestFailure
	artifactSHA string
	cancelled   bool
	manifest    *RunManifest
}

// FileSession represents the conversation records for a single file subtask.
type FileSession struct {
	mu          sync.Mutex
	FilePath    string
	TaskRecords map[TaskType][]*TaskRecord
	session     *SessionHistory // back-reference for JSONL persistence
}

// TaskRecord captures a single LLM request-response cycle within a file subtask.
type TaskRecord struct {
	Type            TaskType
	RequestNo       int           // sequential number within this task type
	RequestMessages []llm.Message // messages sent to LLM
	Response        *ResponseRecord
	ToolResults     []ToolResultRecord
	Duration        time.Duration
	Error           string
	fileSession     *FileSession // back-reference for JSONL persistence
}

// TokenUsage holds token usage for a single LLM request/response cycle.
// Uses actual token counts from the API response when available,
// falling back to local estimation via tiktoken.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// ResponseRecord holds the parsed LLM response.
type ResponseRecord struct {
	Content   string
	ToolCalls []llm.ToolCall
	Model     string
	Usage     *TokenUsage
}

// ToolResultRecord records the result of a tool call executed after the LLM response.
type ToolResultRecord struct {
	ToolName  string
	Arguments string
	Result    string
}

// SessionOptions holds optional metadata for a new session.
type SessionOptions struct {
	ReviewMode  string
	DiffFrom    string
	DiffTo      string
	DiffCommit  string
	ResumedFrom string
	// RunMeta carries cmd-layer-populated manifest metadata. Zero value is
	// fine; the manifest simply omits the corresponding fields.
	RunMeta RunMeta
}

// RunMeta holds the run-identity metadata that only the command layer can
// populate (tool version, resolved provider, config/rules hashes, repo
// identity, resolved range SHAs). It is threaded into SessionOptions and
// folded into the run manifest at Finalize.
type RunMeta struct {
	OCRVersion     string
	Provider       string
	Concurrency    int
	ConfigHash     string
	RulesHash      string
	RepoRemoteURL  string
	RepoHeadSHA    string
	RangeFromSHA   string
	RangeToSHA     string
	RangeCommitSHA string
}

// New creates a new SessionHistory with the given repo directory.
func New(repoDir, gitBranch, model string, opts SessionOptions) *SessionHistory {
	sessionID := generateUUID()
	sh := &SessionHistory{
		SessionID:    sessionID,
		RepoDir:      repoDir,
		GitBranch:    gitBranch,
		Model:        model,
		ReviewMode:   opts.ReviewMode,
		DiffFrom:     opts.DiffFrom,
		DiffTo:       opts.DiffTo,
		DiffCommit:   opts.DiffCommit,
		ResumedFrom:  opts.ResumedFrom,
		StartTime:    time.Now(),
		FileSessions: make(map[string]*FileSession),
		runMeta:      opts.RunMeta,
		selected:     make(map[string]struct{}),
		completed:    make(map[string]struct{}),
		reused:       make(map[string]struct{}),
		failed:       make(map[string]struct{}),
		waived:       make(map[string]struct{}),
	}

	p, err := newJSONLWriter(sessionID, repoDir, gitBranch, model, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ocr session] warning: failed to create session writer: %v\n", err)
	} else {
		sh.persist = p
		p.WriteSessionStart(sh.StartTime)
	}

	return sh
}

// GetOrCreateFileSession returns the FileSession for the given file path,
// creating one if it doesn't exist yet.
func (sh *SessionHistory) GetOrCreateFileSession(filePath string) *FileSession {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	fs, ok := sh.FileSessions[filePath]
	if !ok {
		fs = &FileSession{
			FilePath:    filePath,
			TaskRecords: make(map[TaskType][]*TaskRecord),
			session:     sh,
		}
		sh.FileSessions[filePath] = fs
	}
	return fs
}

// RecordReviewItemDone persists the file-level checkpoint used by resume.
func (sh *SessionHistory) RecordReviewItemDone(filePath, oldPath, newPath, fingerprint string, comments []model.LlmComment) {
	if sh == nil {
		return
	}
	if filePath == "" {
		filePath = newPath
	}
	if filePath != "" {
		sh.GetOrCreateFileSession(filePath)
		sh.trackCoverage("completed", filePath)
	}
	if p := sh.persist; p != nil {
		p.WriteReviewItemDone(filePath, oldPath, newPath, fingerprint, comments)
	}
}

// RecordReviewItemReused records that this run reused a checkpoint from another session.
func (sh *SessionHistory) RecordReviewItemReused(filePath, oldPath, newPath, fingerprint, sourceSessionID string, comments []model.LlmComment) {
	if sh == nil {
		return
	}
	if filePath == "" {
		filePath = newPath
	}
	if filePath != "" {
		sh.GetOrCreateFileSession(filePath)
		sh.trackCoverage("reused", filePath)
	}
	if p := sh.persist; p != nil {
		p.WriteReviewItemReused(filePath, oldPath, newPath, fingerprint, sourceSessionID, comments)
	}
}

// RecordReviewItemFailed persists an incomplete file-level checkpoint. class
// is one of the Failure* constants (see ClassifyFailure); it is recorded in
// the JSONL record and in the run manifest's typed failure list.
func (sh *SessionHistory) RecordReviewItemFailed(filePath, oldPath, newPath, fingerprint, class, errorMsg string) {
	if sh == nil {
		return
	}
	if filePath == "" {
		filePath = newPath
	}
	if filePath != "" {
		sh.GetOrCreateFileSession(filePath)
		sh.mu.Lock()
		sh.failed[filePath] = struct{}{}
		sh.failures = append(sh.failures, ManifestFailure{Path: filePath, Class: class, Error: errorMsg})
		sh.mu.Unlock()
	}
	if p := sh.persist; p != nil {
		p.WriteReviewItemFailed(filePath, oldPath, newPath, fingerprint, class, errorMsg)
	}
}

// RecordReviewItemWaived persists a review_item_waived checkpoint for a diff
// the operator chose to skip on a resumed run. A waive satisfies the coverage
// contract (counts like a completed item) and is reusable by later resumes.
func (sh *SessionHistory) RecordReviewItemWaived(filePath, oldPath, newPath, fingerprint string) {
	if sh == nil {
		return
	}
	if filePath == "" {
		filePath = newPath
	}
	if filePath != "" {
		sh.GetOrCreateFileSession(filePath)
		sh.trackCoverage("waived", filePath)
	}
	if p := sh.persist; p != nil {
		p.WriteReviewItemWaived(filePath, oldPath, newPath, fingerprint)
	}
}

// SetSelected records the full set of reviewable file paths for this run. It
// establishes the coverage denominator: selected is a superset of
// completed ∪ reused ∪ failed ∪ waived, with equality only for fully-covered
// runs. Call it after diffs are computed and before dispatch.
func (sh *SessionHistory) SetSelected(paths []string) {
	if sh == nil {
		return
	}
	sh.mu.Lock()
	for _, p := range paths {
		if p != "" {
			sh.selected[p] = struct{}{}
		}
	}
	sh.mu.Unlock()
}

// SetArtifactChecksum records the checksum identifying the run's input set
// (sha256 over the sorted per-file fingerprints). It proves input identity
// without persisting source.
func (sh *SessionHistory) SetArtifactChecksum(sum string) {
	if sh == nil {
		return
	}
	sh.mu.Lock()
	sh.artifactSHA = sum
	sh.mu.Unlock()
}

// MarkCancelled flags the run as cancelled so ComputeTerminalState degrades
// the state to partial/failed rather than reporting complete.
func (sh *SessionHistory) MarkCancelled() {
	if sh == nil {
		return
	}
	sh.mu.Lock()
	sh.cancelled = true
	sh.mu.Unlock()
}

// trackCoverage adds a path to one of the coverage sets under lock.
func (sh *SessionHistory) trackCoverage(set, path string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	switch set {
	case "completed":
		sh.completed[path] = struct{}{}
	case "reused":
		sh.reused[path] = struct{}{}
	case "waived":
		sh.waived[path] = struct{}{}
	}
}

// Finalize marks the session as complete, sets the end time, and persists
// the final summary record.
func (sh *SessionHistory) Finalize() {
	sh.mu.Lock()
	if sh.manifest != nil {
		// Already finalized. The manifest is written exactly once (its
		// immutability contract), so a second Finalize is a no-op rather than a
		// duplicate session_end + run_manifest pair on an already-closed file.
		sh.mu.Unlock()
		return
	}
	sh.EndTime = time.Now()
	p := sh.persist
	duration := sh.EndTime.Sub(sh.StartTime)
	filesReviewed := make([]string, 0, len(sh.FileSessions))
	for fp := range sh.FileSessions {
		filesReviewed = append(filesReviewed, fp)
	}
	failures := atomic.LoadInt64(&sh.llmFailures)
	manifest := sh.buildManifestLocked(duration)
	sh.manifest = manifest
	sh.mu.Unlock()

	if p != nil {
		// session_end first, then the run_manifest as the final line so the
		// manifest is always the last record (immutability = written once,
		// last); flushAndClose then closes the file.
		p.WriteSessionEnd(duration, filesReviewed, failures)
		p.WriteRunManifest(manifest)
		p.flushAndClose()
	}
}

// buildManifestLocked assembles the immutable run manifest from the tracked
// coverage sets and run metadata. Caller must hold sh.mu.
func (sh *SessionHistory) buildManifestLocked(duration time.Duration) *RunManifest {
	// selected is the coverage denominator; union in the recorded outcomes so
	// the invariant holds even when SetSelected was not called explicitly.
	selected := make(map[string]struct{}, len(sh.selected))
	for p := range sh.selected {
		selected[p] = struct{}{}
	}
	for _, set := range []map[string]struct{}{sh.completed, sh.reused, sh.failed, sh.waived} {
		for p := range set {
			selected[p] = struct{}{}
		}
	}

	state := ComputeTerminalState(len(selected), len(sh.completed), len(sh.reused),
		len(sh.failed), len(sh.waived), sh.cancelled)

	m := &RunManifest{
		SchemaVersion:   ManifestSchemaVersion,
		SessionID:       sh.SessionID,
		ParentSessionID: sh.ResumedFrom,
		Repo: ManifestRepo{
			RemoteURL: sh.runMeta.RepoRemoteURL,
			HeadSHA:   sh.runMeta.RepoHeadSHA,
			Branch:    sh.GitBranch,
			Dir:       sh.RepoDir,
		},
		ReviewMode: sh.ReviewMode,
		Range: ManifestRange{
			From:      sh.DiffFrom,
			To:        sh.DiffTo,
			Commit:    sh.DiffCommit,
			FromSHA:   sh.runMeta.RangeFromSHA,
			ToSHA:     sh.runMeta.RangeToSHA,
			CommitSHA: sh.runMeta.RangeCommitSHA,
		},
		OCRVersion:     sh.runMeta.OCRVersion,
		Provider:       sh.runMeta.Provider,
		Model:          sh.Model,
		Concurrency:    sh.runMeta.Concurrency,
		ConfigHash:     sh.runMeta.ConfigHash,
		RulesHash:      sh.runMeta.RulesHash,
		ArtifactSHA256: sh.artifactSHA,
		Files: ManifestFiles{
			Selected:  sortedKeys(selected),
			Completed: sortedKeys(sh.completed),
			Reused:    sortedKeys(sh.reused),
			Failed:    sortedKeys(sh.failed),
			Waived:    sortedKeys(sh.waived),
		},
		Failures:    append([]ManifestFailure(nil), sh.failures...),
		State:       state,
		StartedAt:   sh.StartTime.UTC().Format(time.RFC3339),
		CompletedAt: sh.EndTime.UTC().Format(time.RFC3339),
		DurationMS:  duration.Milliseconds(),
	}
	return m
}

// Manifest returns the run manifest retained in memory after Finalize, or nil
// if the session has not been finalized yet. The returned value is the same
// data written as the last JSONL record, so JSON output and the persisted
// session expose identical coverage.
func (sh *SessionHistory) Manifest() *RunManifest {
	if sh == nil {
		return nil
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.manifest
}

// AppendTaskRecord adds a new task record to the file session for the given
// file path and task type. It auto-assigns the RequestNo based on existing records
// and writes an llm_request record to the JSONL stream.
func (fs *FileSession) AppendTaskRecord(taskType TaskType, messages []llm.Message) *TaskRecord {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	rec := &TaskRecord{
		Type:            taskType,
		RequestNo:       len(fs.TaskRecords[taskType]) + 1,
		RequestMessages: copyMessages(messages),
		fileSession:     fs,
	}
	fs.TaskRecords[taskType] = append(fs.TaskRecords[taskType], rec)

	if p := fs.session.persist; p != nil {
		p.WriteLLMRequest(fs.FilePath, taskType, rec.RequestNo, copyMessagesForJSON(messages))
	}

	return rec
}

// copyMessages returns a deep copy of a messages slice so that future mutations
// don't corrupt stored records.
func copyMessages(msgs []llm.Message) []llm.Message {
	cp := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		cp[i] = llm.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			ToolCalls:  append([]llm.ToolCall(nil), m.ToolCalls...),
		}
	}
	return cp
}

// copyMessagesForJSON produces a JSON-friendly slice for persistence.
func copyMessagesForJSON(msgs []llm.Message) any {
	type msg struct {
		Role       string `json:"role"`
		Content    any    `json:"content"`
		ToolCallID string `json:"tool_call_id,omitempty"`
	}
	out := make([]msg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msg{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		})
	}
	return out
}

// SetResponse records the LLM response in the most recent TaskRecord of the given type.
// It uses actual token usage from the API response when available, falling back to
// local estimation via tiktoken, and writes an llm_response record to the JSONL stream.
func (tr *TaskRecord) SetResponse(resp *llm.ChatResponse, duration time.Duration) {
	if resp == nil || len(resp.Choices) == 0 {
		tr.SetError(fmt.Errorf("empty response"), duration)
		return
	}
	choice := resp.Choices[0]
	content := ""
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}

	var promptTokens, completionTokens, cacheReadTokens, cacheWriteTokens int
	if resp.Usage != nil {
		promptTokens = int(resp.Usage.PromptTokens)
		completionTokens = int(resp.Usage.CompletionTokens)
		cacheReadTokens = int(resp.Usage.CacheReadTokens)
		cacheWriteTokens = int(resp.Usage.CacheWriteTokens)
	} else {
		for _, m := range tr.RequestMessages {
			promptTokens += llm.CountTokens(m.ExtractText())
		}
		completionTokens = llm.CountTokens(content)
	}

	usage := &TokenUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CacheReadTokens:  cacheReadTokens,
		CacheWriteTokens: cacheWriteTokens,
	}

	tr.Response = &ResponseRecord{
		Content:   content,
		ToolCalls: choice.Message.ToolCalls,
		Model:     resp.Model,
		Usage:     usage,
	}
	tr.Duration = duration

	if fs := tr.fileSession; fs != nil {
		if p := fs.session.persist; p != nil {
			toolCallsJSON := make([]map[string]any, 0, len(choice.Message.ToolCalls))
			for _, tc := range choice.Message.ToolCalls {
				toolCallsJSON = append(toolCallsJSON, map[string]any{
					"id":        tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				})
			}
			p.WriteLLMResponse(fs.FilePath, tr.Type, content, toolCallsJSON, resp.Model, *usage, duration)
		}
	}
}

// SetError records an error for this task record, writes an llm_error entry to
// the JSONL stream, and increments the session-level LLM failure counter.
func (tr *TaskRecord) SetError(err error, duration time.Duration) {
	tr.Error = err.Error()
	tr.Duration = duration

	if fs := tr.fileSession; fs != nil {
		if p := fs.session.persist; p != nil {
			p.WriteLLMError(fs.FilePath, tr.Type, tr.RequestNo, err.Error(), duration)
		}
		atomic.AddInt64(&fs.session.llmFailures, 1)
	}
}

// LLMFailures returns the total number of LLM request failures recorded during this session.
func (sh *SessionHistory) LLMFailures() int64 {
	return atomic.LoadInt64(&sh.llmFailures)
}

// AddToolResult appends a tool call result to this task record and writes a
// tool_call record to the JSONL stream.
func (tr *TaskRecord) AddToolResult(toolName, arguments, result string) {
	tr.ToolResults = append(tr.ToolResults, ToolResultRecord{
		ToolName:  toolName,
		Arguments: arguments,
		Result:    result,
	})

	if fs := tr.fileSession; fs != nil {
		if p := fs.session.persist; p != nil {
			p.WriteToolCall(fs.FilePath, tr.Type, toolName, arguments, result, true, 0)
		}
	}
}
