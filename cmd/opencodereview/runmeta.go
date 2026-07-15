package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-code-review/open-code-review/internal/llm"
	"github.com/open-code-review/open-code-review/internal/session"
)

// configHashInput is the ALLOWLISTED field set folded into config_hash. It
// deliberately excludes every secret-bearing field of the resolved endpoint
// (Token, AuthHeader, ExtraHeaders): the hash must be stable across API-key
// rotations and must never carry a credential. The struct field order is the
// canonical JSON key order, so the marshalled bytes are deterministic.
type configHashInput struct {
	ProviderProtocol string `json:"provider_protocol"`
	Model            string `json:"model"`
	BaseURLHost      string `json:"base_url_host"`
	Language         string `json:"language"`
	TimeoutMS        int64  `json:"timeout_ms"`
}

// buildRunMeta assembles the manifest metadata the command layer owns:
// tool version, provider, config/rules hashes, repo identity, and (for the
// range/commit paths) the resolved range SHAs.
func buildRunMeta(ep llm.ResolvedEndpoint, language, ocrVersion, repoDir, customRulePath string, concurrency int, rng resolvedRange) session.RunMeta {
	remoteURL, headSHA := repoIdentity(repoDir)
	return session.RunMeta{
		OCRVersion:     ocrVersion,
		Provider:       ep.Protocol,
		Concurrency:    concurrency,
		ConfigHash:     computeConfigHash(ep, language),
		RulesHash:      computeRulesHash(customRulePath, repoDir, ocrVersion),
		RepoRemoteURL:  remoteURL,
		RepoHeadSHA:    headSHA,
		RangeFromSHA:   rng.fromSHA,
		RangeToSHA:     rng.toSHA,
		RangeCommitSHA: rng.commitSHA,
	}
}

// computeConfigHash hashes only the allowlisted, non-secret endpoint fields.
func computeConfigHash(ep llm.ResolvedEndpoint, language string) string {
	in := configHashInput{
		ProviderProtocol: ep.Protocol,
		Model:            ep.Model,
		BaseURLHost:      baseURLHost(ep.URL),
		Language:         language,
		TimeoutMS:        ep.Timeout.Milliseconds(),
	}
	data, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// baseURLHost returns the host[:port] of a URL with userinfo AND query
// stripped. Anything unparseable yields "" (best-effort, never a credential).
func baseURLHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	// u.Host excludes userinfo; RawQuery is never included here.
	return u.Host
}

// computeRulesHash hashes each loaded rule layer as (label + \0 + raw bytes),
// in a stable order. Missing layers contribute nothing. The embedded system
// layer has no file, so it contributes label + tool version (it only changes
// when the binary does).
//
// ponytail: reads the rule files by their known paths rather than plumbing raw
// bytes out of rules.NewResolver — same inputs, no new API surface. If rule
// loading grows more layers, mirror them here.
func computeRulesHash(customRulePath, repoDir, ocrVersion string) string {
	h := sha256.New()
	add := func(label, path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		h.Write([]byte(label))
		h.Write([]byte{0})
		h.Write(data)
	}
	if customRulePath != "" {
		add("custom", customRulePath)
	}
	if repoDir != "" {
		add("project", filepath.Join(repoDir, ".opencodereview", "rule.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add("global", filepath.Join(home, ".opencodereview", "rule.json"))
	}
	h.Write([]byte("system\x00" + ocrVersion))
	return hex.EncodeToString(h.Sum(nil))
}

// repoIdentity returns the origin remote URL (with userinfo stripped) and the
// HEAD commit SHA. Both tolerate git failure by returning "".
func repoIdentity(repoDir string) (remoteURL, headSHA string) {
	if repoDir == "" {
		return "", ""
	}
	if out, err := runGitCmdStdout(repoDir, "remote", "get-url", "origin"); err == nil {
		remoteURL = stripURLUserinfo(strings.TrimSpace(string(out)))
	}
	if out, err := runGitCmdStdout(repoDir, "rev-parse", "HEAD"); err == nil {
		headSHA = strings.TrimSpace(string(out))
	}
	return remoteURL, headSHA
}

// stripURLUserinfo removes a user[:password]@ prefix from a standard URL so a
// token embedded in an https remote never lands in the manifest. scp-style
// git remotes (git@host:path) carry no password and parse as opaque, so they
// pass through unchanged.
func stripURLUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// resolvedRange holds the resolved commit SHAs for a review range/commit.
type resolvedRange struct {
	fromSHA   string
	toSHA     string
	commitSHA string
}

// resolveRange rev-parses the review refs to their commit SHAs. It is
// best-effort: unresolved refs simply stay empty. Callers should have already
// validated the refs (validateReviewRefs) before running.
func resolveRange(repoDir, from, to, commit string) resolvedRange {
	var r resolvedRange
	r.fromSHA = revParse(repoDir, from)
	r.toSHA = revParse(repoDir, to)
	r.commitSHA = revParse(repoDir, commit)
	return r
}

func revParse(repoDir, ref string) string {
	if ref == "" {
		return ""
	}
	out, err := runGitCmdStdout(repoDir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
