package workspaces

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The gateway still mints proxy clone URLs with the token in userinfo (so a
// legacy node keeps working). A current node must strip it before handing the
// URL to `git clone`, otherwise git persists the credential into .git/config
// where `git remote -v` exposes it to anything running in the workspace.
func TestSplitGitCredentialURLStripsUserinfo(t *testing.T) {
	token := "task-id.main.rw.deadbeef"
	in := "http://" + token + "@127.0.0.1:8003/api/v1/public/nodes/git/"

	clean, user, pass, ok := SplitGitCredentialURL(in)
	if !ok {
		t.Fatalf("URL with userinfo must split; got ok=false")
	}
	if strings.Contains(clean, token) || strings.Contains(clean, "@") {
		t.Fatalf("clean URL must not carry the credential; got %q", clean)
	}
	if clean != "http://127.0.0.1:8003/api/v1/public/nodes/git/" {
		t.Fatalf("unexpected clean URL: %q", clean)
	}
	if user != token {
		t.Fatalf("username must be the proxy token; got %q", user)
	}
	if pass != "" {
		t.Fatalf("password must be empty for a token-as-username URL; got %q", pass)
	}
}

func TestSplitGitCredentialURLWithUserAndPassword(t *testing.T) {
	clean, user, pass, ok := SplitGitCredentialURL("https://bot:s3cret@git.example.com/o/r.git")
	if !ok {
		t.Fatalf("user:pass URL must split")
	}
	if clean != "https://git.example.com/o/r.git" {
		t.Fatalf("unexpected clean URL: %q", clean)
	}
	if user != "bot" || pass != "s3cret" {
		t.Fatalf("unexpected credential: user=%q pass=%q", user, pass)
	}
}

// A URL with no credential, and a non-http remote (ssh), must pass through
// untouched so the ssh-key path keeps working.
func TestSplitGitCredentialURLPassthrough(t *testing.T) {
	for _, in := range []string{
		"https://git.example.com/o/r.git",
		"git@github.com:owner/repo.git",
		"",
	} {
		clean, user, pass, ok := SplitGitCredentialURL(in)
		if ok {
			t.Fatalf("%q must not report a credential", in)
		}
		if clean != in {
			t.Fatalf("%q must pass through unchanged; got %q", in, clean)
		}
		if user != "" || pass != "" {
			t.Fatalf("%q must yield no credential; got user=%q pass=%q", in, user, pass)
		}
	}
}

// The credential must travel as a per-command `-c http.<origin>.extraHeader`,
// which git does not write to any config file and scopes to the proxy origin so
// a `--recurse-submodules` fetch to a foreign host does not carry it.
func TestGitAuthHeaderArgs(t *testing.T) {
	remote := "http://127.0.0.1:8003/api/v1/public/nodes/git/r/o/r.git/"
	args := GitAuthHeaderArgs("token123", "", remote)
	if len(args) != 2 || args[0] != "-c" {
		t.Fatalf("expected a -c pair; got %v", args)
	}
	want := base64.StdEncoding.EncodeToString([]byte("token123:"))
	if !strings.HasSuffix(args[1], want) {
		t.Fatalf("header must carry base64(token:); got %q", args[1])
	}
	if !strings.HasPrefix(args[1], "http.http://127.0.0.1:8003.extraHeader=Authorization: Basic ") {
		t.Fatalf("header must be origin-scoped to the proxy; got %q", args[1])
	}
}

// A non-http / empty remote URL keeps the historic unscoped header rather than
// silently losing authentication.
func TestGitAuthHeaderArgsUnscopedWithoutHTTPRemote(t *testing.T) {
	for _, remote := range []string{"", "git@github.com:owner/repo.git"} {
		args := GitAuthHeaderArgs("token123", "", remote)
		if !strings.HasPrefix(args[1], "http.extraHeader=Authorization: Basic ") {
			t.Fatalf("remote %q must fall back to the unscoped header; got %v", remote, args)
		}
	}
}

func TestGitAuthHeaderArgsEmptyWithoutCredential(t *testing.T) {
	if args := GitAuthHeaderArgs("", "", ""); args != nil {
		t.Fatalf("no credential must yield no args; got %v", args)
	}
}

// The auth args must precede the subcommand: `git -c ... clone ...`.
func TestAuthArgsPrecedeCloneSubcommand(t *testing.T) {
	auth := GitAuthHeaderArgs("tok", "", "http://127.0.0.1:8003/api/v1/public/nodes/git/")
	full := append(auth, GitCloneArgs("https://git.example.com/o/r.git", GitWorkspaceConfig{}, "/tmp/wd")...)
	if full[0] != "-c" {
		t.Fatalf("auth args must come first; got %v", full)
	}
	if full[2] != "clone" {
		t.Fatalf("subcommand must follow the -c pair; got %v", full)
	}
}

// RedactGitSecrets must blank the scoped header's credential too, not only the
// plain unscoped form.
func TestRedactGitSecretsBlanksScopedAuthHeader(t *testing.T) {
	in := "git -c http.http://127.0.0.1:8003.extraHeader=Authorization: Basic dG9rMTIzOg== clone ..."
	out := RedactGitSecrets(in)
	if strings.Contains(out, "dG9rMTIzOg==") {
		t.Fatalf("credential must be redacted; got %q", out)
	}
	if !strings.Contains(out, "http://127.0.0.1:8003.extraHeader=Authorization: Basic ***") {
		t.Fatalf("redaction must leave the scope visible; got %q", out)
	}
}
