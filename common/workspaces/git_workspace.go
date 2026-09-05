// Package workspaces holds the git-provisioning helpers a node uses to lay down
// a session's working tree before the provider CLI runs against it.
//
// This is the trimmed, node-side subset of the original agent-compose
// pkg/workspaces: only the pure helpers the node agent calls remain
// (GitWorkspaceConfig, ApplyGitCredentials, HostWorkspaceInitialized,
// GitCloneArgs, GitCommitFetchArgs). The server-only Prepare* paths — which
// pulled in pkg/model and its driver/loader/storage closure — are intentionally
// dropped so the node binaries cross-compile with no daemon dependencies.
package workspaces

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// GitWorkspaceTempDirName is the scratch dir a root clone lands in before being
// promoted. The node treats it (like .agent-compose) as non-user content when
// deciding whether a working tree is already initialized.
const GitWorkspaceTempDirName = ".agent-compose-git-clone"

// GitWorkspaceConfig describes how to provision a session's working tree from a
// git repository. It is populated from the NodeGitSpec on the wire.
type GitWorkspaceConfig struct {
	URL          string `json:"url"`
	Branch       string `json:"branch,omitempty"`
	Commit       string `json:"commit,omitempty"`
	Credential   string `json:"credential,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	CloneTarget  string `json:"path,omitempty"`
	CreateBranch bool   `json:"create_branch,omitempty"`
	NewBranch    string `json:"new_branch,omitempty"`
}

// HostWorkspaceInitialized reports whether workspaceRoot already holds a checkout
// (any entry other than the node's own runtime/scratch dirs), so a reconnect or
// recreate skips re-cloning. A missing directory is reported as "not initialized"
// (false, nil) rather than an error: a fresh session has no workDir yet, and the
// caller (provisionGit) will let `git clone` create it. Any other read failure is
// a genuine error the caller must surface.
func HostWorkspaceInitialized(workspaceRoot string) (bool, error) {
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read workspace root: %w", err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case ".agent-compose", GitWorkspaceTempDirName:
			continue
		}
		return true, nil
	}
	return false, nil
}

// ResetStaleWorkspace removes a workDir that holds no real checkout so a shallow
// clone has a clean target. It is the complement to HostWorkspaceInitialized: the
// caller has already confirmed the dir is NOT initialized, so the only content
// here is scratch (``.agent-compose`` / ``.agent-compose-git-clone``) or a partial
// clone left by a failed prior provision — never a real checkout. A missing dir is
// a no-op (git clone will create it).
func ResetStaleWorkspace(workspaceRoot string) error {
	if _, err := os.Stat(workspaceRoot); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat workspace root: %w", err)
	}
	if err := os.RemoveAll(workspaceRoot); err != nil {
		return fmt.Errorf("reset stale workspace %s: %w", workspaceRoot, err)
	}
	return nil
}

// GitCloneArgs builds the `git clone` argument vector for a shallow clone of
// cloneURL into clonePath, honoring the requested branch.
//
// `--recurse-submodules` pulls the repo's submodules in the same pass. This
// repository keeps its own components as submodules (node_server / nodes /
// user-frontend / mobile / device-control), so a clone without it produced a
// workspace whose submodule directories were empty stubs — the agent then saw a
// tree that could not build. `--shallow-submodules` keeps each submodule at
// depth 1, matching the parent's shallow clone instead of dragging in full
// submodule history.
func GitCloneArgs(cloneURL string, cfg GitWorkspaceConfig, clonePath string) []string {
	args := []string{"clone", "--depth", "1", "--recurse-submodules", "--shallow-submodules"}
	if branch := strings.TrimSpace(cfg.Branch); branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, cloneURL, clonePath)
	return args
}

// GitCommitFetchArgs builds the `git fetch` argument vector that pulls a single
// commit into a shallow clone.
func GitCommitFetchArgs(commit string) []string {
	return []string{"fetch", "--depth", "1", "origin", commit}
}

// ApplyGitCredentials injects basic-auth credentials into an http(s) clone URL
// when the config carries them and the URL does not already embed a userinfo
// component. Non-http URLs and already-authenticated URLs are returned as-is.
//
// DEPRECATED for the session-repo path: a credential embedded here is persisted
// by `git clone` into .git/config as remote.origin.url, where any process in the
// workspace (including the agent itself, and any other user sharing the host)
// can read it back with `git remote -v`. Session provisioning now uses
// SplitGitCredentialURL + GitAuthHeaderArgs so the credential travels in a
// per-command header and never lands on disk. Retained for the resource/skill
// download path, which fetches over plain HTTP without a persisted remote.
func ApplyGitCredentials(cloneURL string, cfg GitWorkspaceConfig) string {
	trimmedURL := strings.TrimSpace(cloneURL)
	if trimmedURL == "" {
		return ""
	}
	credential := strings.TrimSpace(cfg.Credential)
	if credential == "" {
		user := strings.TrimSpace(cfg.Username)
		pass := strings.TrimSpace(cfg.Password)
		if user != "" || pass != "" {
			credential = url.QueryEscape(user) + ":" + url.QueryEscape(pass)
		}
	}
	if credential == "" {
		return trimmedURL
	}
	if strings.Contains(trimmedURL, "@") {
		return trimmedURL
	}
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(trimmedURL, prefix) {
			return prefix + credential + "@" + strings.TrimPrefix(trimmedURL, prefix)
		}
	}
	return trimmedURL
}

// SplitGitCredentialURL separates an http(s) clone URL into a credential-free URL
// and the basic-auth pair embedded in its userinfo.
//
// This is the inverse of the historic ApplyGitCredentials: the gateway still
// mints proxy URLs of the form ``http://<token>@host/path`` (so a legacy node
// keeps working), but a current node strips the userinfo before handing the URL
// to `git clone` and re-supplies it as a header. `.git/config` therefore records
// a clean remote, and `git remote -v` exposes no secret.
//
// Returns ok=false (and the URL unchanged) for non-http(s) URLs, unparsable
// URLs, or URLs carrying no userinfo.
func SplitGitCredentialURL(cloneURL string) (cleanURL, username, password string, ok bool) {
	trimmed := strings.TrimSpace(cloneURL)
	if trimmed == "" {
		return cloneURL, "", "", false
	}
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		return trimmed, "", "", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.User == nil {
		return trimmed, "", "", false
	}
	user := parsed.User.Username()
	pass, _ := parsed.User.Password()
	if user == "" && pass == "" {
		return trimmed, "", "", false
	}
	parsed.User = nil
	return parsed.String(), user, pass, true
}

// GitAuthHeaderArgs returns the `git -c` prefix that authenticates a single git
// command with an Authorization header.
//
// Passed per-invocation (before the subcommand) it is never written to any git
// config file, so the credential does not outlive the process. A token with no
// username authenticates as the username half with an empty password, matching
// the PAT-as-username form GitHub/Gitea/GitLab accept and what the git-proxy
// expects from a clone URL's userinfo.
//
// The header is scoped to remoteURL's origin (``http.<origin>.extraHeader``)
// rather than set unconditionally: ``git clone --recurse-submodules`` contacts
// whatever hosts the superproject's ``.gitmodules`` names, and an unscoped
// header would hand this credential to every one of them. Git's ``http.<url>.*``
// matching considers scheme/host/port (userinfo ignored), so one origin-scoped
// key covers the main repo and every relative submodule that resolves back into
// the same proxy. remoteURL is the credential-free URL (userinfo stripped) the
// command will actually use; empty/unsupported URLs keep the historic
// unscoped behavior rather than silently losing authentication.
//
// Note the residual exposure: -c values appear in the clone process's argv, so a
// process listing on the same host can observe them for the clone's duration.
// That is strictly narrower than the previous behavior (a credential persisted
// in .git/config for the lifetime of the workspace).
func GitAuthHeaderArgs(username, password, remoteURL string) []string {
	user := strings.TrimSpace(username)
	pass := strings.TrimSpace(password)
	if user == "" && pass == "" {
		return nil
	}
	credential := user + ":" + pass
	encoded := base64.StdEncoding.EncodeToString([]byte(credential))
	key := "http.extraHeader"
	if origin := gitConfigURLOrigin(remoteURL); origin != "" {
		key = "http." + origin + ".extraHeader"
	}
	return []string{"-c", key + "=Authorization: Basic " + encoded}
}

// gitConfigURLOrigin reduces an http(s) URL to the ``scheme://host[:port]``
// form used in a ``http.<url>.*`` config key: origin-wide scope covers the main
// repo and every relative submodule URL that resolves back to the same proxy.
// Returns "" for non-http(s) or unparsable URLs (caller falls back to the
// unscoped key).
func gitConfigURLOrigin(remoteURL string) string {
	trimmed := strings.TrimSpace(remoteURL)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	parsed.User = nil
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// redactURLUserInfo matches an http(s) URL's authority userinfo (everything
// between "scheme://" and the first "@" before the path/query/fragment) so it
// can be blanked without touching the rest of the URL or non-credential URLs.
var redactURLUserInfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)([^/?#]*)@`)

// redactAuthHeaderArg matches the value of a `http.[url.]extraHeader=Authorization:`
// git config assignment (as produced by GitAuthHeaderArgs) up to the next
// whitespace, so the base64 credential can be blanked. Without this the
// credential would surface in any error text that echoes the git argv — the same
// exposure redactURLUserInfo closes for URL-embedded credentials. The URL-scoped
// form (``http.http://host.extraHeader=...``) is matched too: the middle URL may
// contain any characters except the ``=`` that starts the value.
var redactAuthHeaderArg = regexp.MustCompile(`(http\.[^=\s]*extraHeader=Authorization:\s*\S+\s+)\S+`)

// RedactGitURL strips the userinfo (credentials) from every http(s) URL
// appearing in s, replacing the segment before the first "@" in the authority
// with "***". URLs without embedded credentials are returned unchanged. The
// node formats git errors through this so a clone URL carrying a proxy token
// (or any embedded PAT) never leaks into ack/error/log surfaces.
func RedactGitURL(s string) string {
	return redactURLUserInfo.ReplaceAllString(s, "${1}***@")
}

// RedactGitSecrets blanks every credential shape the node can put on a git
// command line: a URL-embedded userinfo AND an `http.extraHeader` basic-auth
// value. Error/ack/log surfaces must go through this rather than RedactGitURL
// alone, since session provisioning now passes the credential as a header.
func RedactGitSecrets(s string) string {
	return redactAuthHeaderArg.ReplaceAllString(RedactGitURL(s), "${1}***")
}
