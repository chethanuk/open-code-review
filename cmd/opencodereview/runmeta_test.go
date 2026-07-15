package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-code-review/open-code-review/internal/config/rules"
	"github.com/open-code-review/open-code-review/internal/llm"
)

func TestConfigHashRedaction(t *testing.T) {
	base := llm.ResolvedEndpoint{
		URL:        "https://user:supersecret@api.example.com/v1/chat/completions?key=leak",
		Token:      "sk-ant-TOPSECRET",
		Model:      "claude-opus-4-6",
		Protocol:   llm.ProtocolAnthropic,
		AuthHeader: "x-api-key",
		Timeout:    30 * time.Second,
	}
	// Rotating the token / auth header / extra headers must NOT change the hash.
	rotated := base
	rotated.Token = "sk-ant-DIFFERENT"
	rotated.AuthHeader = "authorization"
	rotated.ExtraHeaders = map[string]string{"x-custom": "value"}

	h1 := computeConfigHash(base, "English")
	h2 := computeConfigHash(rotated, "English")
	if h1 != h2 {
		t.Errorf("config hash changed on token/header rotation: %q vs %q", h1, h2)
	}

	// Changing an allowlisted field MUST change the hash.
	if computeConfigHash(base, "Chinese") == h1 {
		t.Error("config hash should change when language changes")
	}
	m2 := base
	m2.Model = "claude-sonnet-4-6"
	if computeConfigHash(m2, "English") == h1 {
		t.Error("config hash should change when model changes")
	}

	// The hash input must never contain the token or userinfo/query.
	in := configHashInput{
		ProviderProtocol: base.Protocol,
		Model:            base.Model,
		BaseURLHost:      baseURLHost(base.URL),
		Language:         "English",
		TimeoutMS:        base.Timeout.Milliseconds(),
	}
	blob := strings.ToLower(in.ProviderProtocol + in.Model + in.BaseURLHost)
	for _, bad := range []string{"supersecret", "sk-ant", "topsecret", "key=leak", "user:"} {
		if strings.Contains(blob, strings.ToLower(bad)) {
			t.Errorf("config hash input leaks %q: %+v", bad, in)
		}
	}
	if in.BaseURLHost != "api.example.com" {
		t.Errorf("base_url_host = %q, want api.example.com (userinfo+query stripped)", in.BaseURLHost)
	}
}

func TestRedactRemoteURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://user:token@github.com/acme/repo.git", "https://github.com/acme/repo.git"},
		{"https://github.com/acme/repo.git", "https://github.com/acme/repo.git"},
		{"https://github.com/acme/repo.git?access_token=SECRET", "https://github.com/acme/repo.git"},
		{"https://user:token@github.com/acme/repo.git?access_token=SECRET#frag", "https://github.com/acme/repo.git"},
		{"git@github.com:acme/repo.git", "git@github.com:acme/repo.git"}, // scp-style: no password, unchanged
		{"", ""},
	}
	for _, tt := range tests {
		if got := redactRemoteURL(tt.in); got != tt.want {
			t.Errorf("redactRemoteURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// gitInitRepo creates a throwaway git repo with an origin remote carrying
// embedded credentials and one commit. Returns the repo dir.
func gitInitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "https://alice:secrettoken@example.com/acme/repo.git?access_token=querysecret")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

func TestRepoIdentityStripsUserinfo(t *testing.T) {
	dir := gitInitRepo(t)
	remoteURL, headSHA := repoIdentity(dir)
	if strings.Contains(remoteURL, "secrettoken") || strings.Contains(remoteURL, "alice:") || strings.Contains(remoteURL, "querysecret") {
		t.Errorf("repo remote URL leaks credentials: %q", remoteURL)
	}
	if remoteURL != "https://example.com/acme/repo.git" {
		t.Errorf("remote URL = %q", remoteURL)
	}
	if len(headSHA) != 40 {
		t.Errorf("head SHA = %q, want 40-char sha", headSHA)
	}
}

func TestResolveRangeReturnsSHAs(t *testing.T) {
	dir := gitInitRepo(t)
	head := revParse(dir, "HEAD")
	if len(head) != 40 {
		t.Fatalf("HEAD sha = %q", head)
	}
	// commit mode: commitSHA resolved, from/to empty.
	r := resolveRange(dir, "", "", "HEAD")
	if r.commitSHA != head {
		t.Errorf("commitSHA = %q, want %q", r.commitSHA, head)
	}
	if r.fromSHA != "" || r.toSHA != "" {
		t.Errorf("from/to should be empty for commit mode: %+v", r)
	}
	// unknown ref resolves to empty, never errors.
	if got := revParse(dir, "does-not-exist"); got != "" {
		t.Errorf("unknown ref = %q, want empty", got)
	}
}

func TestComputeRulesHashChangesWithLayer(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate the global ~/.opencodereview layer
	dir := t.TempDir()
	newResolver := func() rules.Resolver {
		t.Helper()
		r, _, err := rules.NewResolver(dir, "")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	writeFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h1 := computeRulesHash(newResolver(), "1.0.0")

	// Adding a project rule layer changes the hash.
	rulesDir := filepath.Join(dir, ".opencodereview")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(filepath.Join(rulesDir, "rule.json"), `{"rules":[{"path":"**/*.go","rule":"team.md"}]}`)
	writeFile(filepath.Join(dir, "team.md"), "rule text A")
	h2 := computeRulesHash(newResolver(), "1.0.0")
	if h1 == h2 {
		t.Error("rules hash should change when a rule layer appears")
	}

	// Editing the referenced rule file changes the effective rules, so the
	// hash must change even though rule.json's own bytes are unchanged.
	writeFile(filepath.Join(dir, "team.md"), "rule text B")
	h3 := computeRulesHash(newResolver(), "1.0.0")
	if h3 == h2 {
		t.Error("rules hash should change when a referenced rule file changes")
	}

	// Version bump changes the embedded-system contribution.
	if computeRulesHash(newResolver(), "2.0.0") == h3 {
		t.Error("rules hash should change when tool version changes")
	}
}
