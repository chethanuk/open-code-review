package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/open-code-review/open-code-review/internal/config/rules"
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
func buildRunMeta(ep llm.ResolvedEndpoint, language, ocrVersion, repoDir string, resolver rules.Resolver, concurrency int, rng resolvedRange) session.RunMeta {
	remoteURL, headSHA := repoIdentity(repoDir)
	return session.RunMeta{
		OCRVersion:     ocrVersion,
		Provider:       ep.Protocol,
		Concurrency:    concurrency,
		ConfigHash:     computeConfigHash(ep, language),
		RulesHash:      computeRulesHash(resolver, ocrVersion),
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

// computeRulesHash hashes each loaded rule layer as (label + \0 + canonical
// JSON of the resolved layer), in a stable order. "Resolved" means rule
// entries that reference files (e.g. a rule.json entry pointing at team.md)
// have already been expanded to that file's content by rules.NewResolver, so
// the hash tracks the effective runtime rules — editing a referenced file
// changes the hash even though rule.json's bytes do not. Missing layers
// contribute nothing. The embedded system layer has no file, so it
// contributes label + tool version (it only changes when the binary does).
func computeRulesHash(resolver rules.Resolver, ocrVersion string) string {
	h := sha256.New()
	add := func(label string, layer *rules.ProjectRule) {
		if layer == nil {
			return
		}
		data, err := json.Marshal(layer)
		if err != nil {
			return
		}
		h.Write([]byte(label))
		h.Write([]byte{0})
		h.Write(data)
	}
	if ul, ok := resolver.(interface {
		UserLayers() (custom, project, global *rules.ProjectRule)
	}); ok {
		custom, project, global := ul.UserLayers()
		add("custom", custom)
		add("project", project)
		add("global", global)
	}
	h.Write([]byte("system\x00" + ocrVersion))
	return hex.EncodeToString(h.Sum(nil))
}

// repoIdentity returns the origin remote URL (with userinfo/query redacted)
// and the HEAD commit SHA. Both tolerate git failure by returning "".
func repoIdentity(repoDir string) (remoteURL, headSHA string) {
	if repoDir == "" {
		return "", ""
	}
	if out, err := runGitCmdStdout(repoDir, "remote", "get-url", "origin"); err == nil {
		remoteURL = redactRemoteURL(strings.TrimSpace(string(out)))
	}
	if out, err := runGitCmdStdout(repoDir, "rev-parse", "HEAD"); err == nil {
		headSHA = strings.TrimSpace(string(out))
	}
	return remoteURL, headSHA
}

// redactRemoteURL removes the user[:password]@ prefix and any query/fragment
// from a standard URL so a token embedded in an https remote (userinfo or
// ?access_token=…) never lands in the manifest, matching baseURLHost's
// redaction of the config-hash input. scp-style git remotes (git@host:path)
// fail url.Parse, carry no password, and pass through unchanged.
func redactRemoteURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
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
