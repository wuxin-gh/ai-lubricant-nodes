package workspaces

import (
	"strings"
	"testing"
)

// RedactGitURL must blank the userinfo of an http(s) URL while leaving the
// host, path, and query intact — the proxy token embedded as userinfo must
// never reach git error / ack / log surfaces.
func TestRedactGitURLBlanksUserinfo(t *testing.T) {
	cases := map[string]string{
		"http://tok@127.0.0.1:8003/api/v1/public/nodes/git/":                "http://***@127.0.0.1:8003/api/v1/public/nodes/git/",
		"https://user:pat@gitea.example.com/owner/repo.git/info/refs":       "https://***@gitea.example.com/owner/repo.git/info/refs",
		"https://tok@gitlab.com/group/repo":                                 "https://***@gitlab.com/group/repo",
		"plain error: fatal: could not read Username":                      "plain error: fatal: could not read Username",
		"http://127.0.0.1:8003/api/v1/public/nodes/git/":                    "http://127.0.0.1:8003/api/v1/public/nodes/git/",
		"clone --depth 1 http://tok@host/repo.git /workdir failed":          "clone --depth 1 http://***@host/repo.git /workdir failed",
	}
	for in, want := range cases {
		if got := RedactGitURL(in); got != want {
			t.Errorf("RedactGitURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A URL with no userinfo is left unchanged, including one whose path or query
// contains an "@" (the redactor only touches the authority, never the path).
func TestRedactGitURLLeavesNonCredentialURLsAlone(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8003/api/v1/public/nodes/git/info/refs?service=git-upload-pack": "http://127.0.0.1:8003/api/v1/public/nodes/git/info/refs?service=git-upload-pack",
		"https://example.com/path/with/@/segments": "https://example.com/path/with/@/segments",
		"ssh://git@example.com/repo.git":            "ssh://***@example.com/repo.git",
	}
	// Note: ssh:// is matched too — any "scheme://" with a userinfo segment is
	// redacted. That is acceptable: the node only ever clones http(s) here, and
	// redacting an ssh URL's user is harmless.
	for in, want := range cases {
		if got := RedactGitURL(in); got != want {
			t.Errorf("RedactGitURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// Multiple credential-bearing URLs in one string are all redacted.
func TestRedactGitURLReplacesAllOccurrences(t *testing.T) {
	in := "clone http://tok@host/a.git && fetch http://tok@host/b.git"
	want := "clone http://***@host/a.git && fetch http://***@host/b.git"
	if got := RedactGitURL(in); got != want {
		t.Errorf("RedactGitURL(%q) = %q, want %q", in, got, want)
	}
}

// Session provisioning passes the credential as `-c http.extraHeader=...`, so
// the argv echoed in an error carries a base64 basic-auth value rather than a
// URL userinfo. RedactGitSecrets must blank that too, while keeping the clean
// clone URL visible so an operator can still see which remote failed.
func TestRedactGitSecretsBlanksAuthHeader(t *testing.T) {
	in := "git -c http.extraHeader=Authorization: Basic U1VQRVJTRUNSRVQ6 clone --depth 1 http://127.0.0.1:1/nope/repo.git /wd failed"
	got := RedactGitSecrets(in)
	if strings.Contains(got, "U1VQRVJTRUNSRVQ6") {
		t.Errorf("credential leaked: %q", got)
	}
	if !strings.Contains(got, "Basic ***") {
		t.Errorf("expected the header value blanked, got %q", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:1/nope/repo.git") {
		t.Errorf("clean clone URL must remain visible, got %q", got)
	}
}

// RedactGitSecrets is a superset of RedactGitURL: a URL-embedded credential is
// still blanked (legacy nodes / the resource download path).
func TestRedactGitSecretsAlsoBlanksURLUserinfo(t *testing.T) {
	got := RedactGitSecrets("clone http://tok@host/a.git")
	if got != "clone http://***@host/a.git" {
		t.Errorf("unexpected redaction: %q", got)
	}
}

func TestRedactGitSecretsLeavesCleanTextAlone(t *testing.T) {
	in := "git clone --depth 1 https://example.com/o/r.git /wd failed: not found"
	if got := RedactGitSecrets(in); got != in {
		t.Errorf("clean text must pass through; got %q", got)
	}
}
