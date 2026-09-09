package build

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// uploadArtifact POSTs one built artifact to the server's one-time upload
// endpoint. The token is bound to build_id on the server (issue_upload_token,
// module="wda_build", TTL 600s), single use, and only valid for that build.
//
// The body is the RAW artifact bytes (Content-Type application/octet-stream):
// the server reads request.body() verbatim and rejects anything that does not
// start with the zip "PK" magic — a multipart envelope would fail that check
// AND poison the stored file, so no form wrapping is used. Both Bearer and
// ?token= fallback are supported so a server that rejects the header on a
// public route still authenticates the file.
func uploadArtifact(ctx context.Context, uploadURL, token, path, filename string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	finalURL, err := withTokenQuery(uploadURL, token)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, finalURL, f)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))

	client := &http.Client{Timeout: 0} // timeout governed by ctx; uploads can be large
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("upload rejected: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// withTokenQuery adds a ?token= query param to a URL that lacks one, so a token
// can be conveyed on a public route that strips the Authorization header. A
// URL that already carries ?token= is left alone.
func withTokenQuery(raw, token string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if q.Get("token") == "" {
		q.Set("token", token)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}
