package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// maxUploadChunkBytes caps a single chunk's payload; the server enforces the
// same limit before forwarding. 1 MiB keeps each node-protocol frame modest.
const maxUploadChunkBytes = 1024 * 1024

// maxUploadTotalBytes caps a full upload. The browser enforces 10 MiB; the node
// rejects anything larger so a misbehaving caller cannot fill the disk.
const maxUploadTotalBytes = 10 * 1024 * 1024

// uploadSession tracks an in-progress upload to a temp file.
type uploadSession struct {
	mu       sync.Mutex
	tmpPath  string
	file     *os.File
	written  uint64
	uploadID string
}

var (
	uploadSessions   = make(map[string]*uploadSession)
	uploadSessionsMu sync.Mutex
)

// RunFileUpload processes one chunk of a host file upload. Chunks for the same
// upload_id append to `{path}.upload-{upload_id}.tmp`; the chunk with final=true
// verifies total_size + sha256 and atomically renames to path. Each call returns
// a per-chunk ack; a failed ack drops the temp file and the caller must restart
// with a fresh upload_id.
func RunFileUpload(ctx context.Context, req *agentcomposev2.NodeFileUploadRequest) *agentcomposev2.NodeFileUploadResult {
	result := &agentcomposev2.NodeFileUploadResult{UploadId: req.GetUploadId(), Path: req.GetPath()}
	uploadID := strings.TrimSpace(req.GetUploadId())
	dest := strings.TrimSpace(req.GetPath())
	if uploadID == "" || dest == "" {
		result.Error = "upload_id and path are required"
		return result
	}
	if err := validateUploadPath(dest); err != nil {
		result.Error = err.Error()
		return result
	}
	if req.GetTotalSize() > maxUploadTotalBytes {
		result.Error = fmt.Sprintf("upload exceeds %d byte limit", maxUploadTotalBytes)
		return result
	}
	if len(req.GetData()) > maxUploadChunkBytes {
		result.Error = fmt.Sprintf("chunk exceeds %d byte limit", maxUploadChunkBytes)
		return result
	}

	uploadSessionsMu.Lock()
	sess, ok := uploadSessions[uploadID]
	if !ok {
		tmpPath := dest + ".upload-" + sanitizeUploadID(uploadID) + ".tmp"
		f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			uploadSessionsMu.Unlock()
			result.Error = fmt.Sprintf("create temp file: %v", err)
			return result
		}
		sess = &uploadSession{tmpPath: tmpPath, file: f, uploadID: uploadID}
		uploadSessions[uploadID] = sess
	}
	uploadSessionsMu.Unlock()

	sess.mu.Lock()
	defer sess.mu.Unlock()

	// A concurrent failure already closed this session; reject further chunks.
	if sess.file == nil {
		result.Error = "upload session closed; restart with a new upload_id"
		return result
	}

	if req.GetOffset() != sess.written {
		result.Error = fmt.Sprintf("offset mismatch: expected %d got %d", sess.written, req.GetOffset())
		abortUpload(sess)
		return result
	}

	select {
	case <-ctx.Done():
		result.Error = "canceled"
		abortUpload(sess)
		return result
	default:
	}

	if len(req.GetData()) > 0 {
		n, err := sess.file.Write(req.GetData())
		if err != nil {
			result.Error = fmt.Sprintf("write: %v", err)
			abortUpload(sess)
			return result
		}
		sess.written += uint64(n)
	}

	result.BytesWritten = sess.written
	result.Ok = true

	if !req.GetFinal() {
		return result
	}

	if sess.written != req.GetTotalSize() {
		result.Error = fmt.Sprintf("size mismatch: expected %d got %d", req.GetTotalSize(), sess.written)
		abortUpload(sess)
		result.Ok = false
		return result
	}
	if hash := req.GetSha256(); hash != "" {
		if err := verifyUploadChecksum(sess.tmpPath, hash); err != nil {
			result.Error = err.Error()
			abortUpload(sess)
			result.Ok = false
			return result
		}
	}
	if err := sess.file.Close(); err != nil {
		result.Error = fmt.Sprintf("close temp: %v", err)
		abortUpload(sess)
		result.Ok = false
		return result
	}
	sess.file = nil
	if !req.GetOverwrite() {
		if _, err := os.Stat(dest); err == nil {
			_ = os.Remove(sess.tmpPath)
			dropUploadSession(sess)
			result.Error = "destination exists and overwrite is false"
			result.Ok = false
			return result
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(sess.tmpPath)
			dropUploadSession(sess)
			result.Error = fmt.Sprintf("stat destination: %v", err)
			result.Ok = false
			return result
		}
	}
	if err := os.Rename(sess.tmpPath, dest); err != nil {
		_ = os.Remove(sess.tmpPath)
		dropUploadSession(sess)
		result.Error = fmt.Sprintf("rename: %v", err)
		result.Ok = false
		return result
	}
	dropUploadSession(sess)
	result.Ok = true
	return result
}

func abortUpload(sess *uploadSession) {
	if sess.file != nil {
		_ = sess.file.Close()
		sess.file = nil
	}
	_ = os.Remove(sess.tmpPath)
	dropUploadSession(sess)
}

func dropUploadSession(sess *uploadSession) {
	uploadSessionsMu.Lock()
	if cur, ok := uploadSessions[sess.uploadID]; ok && cur == sess {
		delete(uploadSessions, sess.uploadID)
	}
	uploadSessionsMu.Unlock()
}

func verifyUploadChecksum(tmpPath, expectedHex string) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open for checksum: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("checksum read: %v", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expectedHex) {
		return fmt.Errorf("checksum mismatch: expected %s got %s", expectedHex, got)
	}
	return nil
}

// validateUploadPath confines uploads to an absolute path with no traversal,
// UNC/device paths, Windows reserved names, or drive-relative forms. It does
// not anchor to a configured root (the admin caller is trusted and the route
// reuses the existing node file path validators).
func validateUploadPath(p string) error {
	if p == "" {
		return errors.New("path is required")
	}
	if strings.ContainsRune(p, 0) {
		return errors.New("invalid path")
	}
	cleaned := filepath.Clean(p)
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
			return errors.New("UNC paths are not allowed")
		}
		vol := filepath.VolumeName(cleaned)
		if vol == "" || !strings.HasSuffix(vol, ":") {
			return errors.New("absolute drive path required")
		}
		if !filepath.IsAbs(cleaned) {
			return errors.New("absolute path required")
		}
	} else {
		if !filepath.IsAbs(cleaned) {
			return errors.New("absolute path required")
		}
		if strings.HasPrefix(cleaned, "//") {
			return errors.New("invalid path")
		}
	}
	for _, part := range strings.Split(filepath.ToSlash(cleaned), "/") {
		if part == ".." {
			return errors.New("path traversal is not allowed")
		}
		if isWindowsReservedName(part) {
			return errors.New("reserved name is not allowed")
		}
	}
	return nil
}

func isWindowsReservedName(name string) bool {
	if name == "" {
		return false
	}
	stem := name
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		stem = name[:i]
	}
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

func sanitizeUploadID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "upload"
	}
	return b.String()
}
