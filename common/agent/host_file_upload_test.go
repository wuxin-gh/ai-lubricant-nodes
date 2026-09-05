package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// sha256Hex hashes data and returns the lowercase hex digest. (Use this
// instead of sha256.New().Sum(data), which does NOT hash data — it appends
// data to the empty hash.)
func sha256Hex(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// tmpDest builds a unique absolute destination under the test temp dir so the
// suite never collides with a real file. Returns the absolute path.
func tmpDest(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	dest := filepath.Join(dir, name)
	return dest
}

func TestRunFileUploadSingleChunkAtomicRename(t *testing.T) {
	dest := tmpDest(t, "report.bin")
	data := bytes.Repeat([]byte{0x7f}, 4096)

	res := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId:   "up-1",
		Path:       dest,
		Offset:     0,
		Data:       data,
		TotalSize:  uint64(len(data)),
		Sha256:     sha256Hex(data),
		Overwrite:  true,
		Final:      true,
	})

	if !res.GetOk() {
		t.Fatalf("expected ok, got error %q", res.GetError())
	}
	if res.GetBytesWritten() != uint64(len(data)) {
		t.Fatalf("bytes_written = %d, want %d", res.GetBytesWritten(), uint64(len(data)))
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("dest content mismatch")
	}
	// The temp file must be gone after the atomic rename.
	if _, err := os.Stat(dest + ".upload-up-1.tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestRunFileUploadMultipleChunksAppend(t *testing.T) {
	dest := tmpDest(t, "multi.bin")
	data := make([]byte, 0)
	chunks := [][]byte{
		bytes.Repeat([]byte{'A'}, 1000),
		bytes.Repeat([]byte{'B'}, 1000),
		bytes.Repeat([]byte{'C'}, 500),
	}
	for _, c := range chunks {
		data = append(data, c...)
	}
	uploadID := "up-multi"
	var offset uint64
	for i, c := range chunks {
		isFinal := i == len(chunks)-1
		req := &agentcomposev2.NodeFileUploadRequest{
			UploadId:  uploadID,
			Path:      dest,
			Offset:    offset,
			Data:      c,
			TotalSize: uint64(len(data)),
			Overwrite: true,
			Final:     isFinal,
		}
		if isFinal {
			req.Sha256 = sha256Hex(data)
		}
		res := RunFileUpload(context.Background(), req)
		if !res.GetOk() {
			t.Fatalf("chunk %d: expected ok, got %q", i, res.GetError())
		}
		offset += uint64(len(c))
		if res.GetBytesWritten() != offset {
			t.Fatalf("chunk %d: bytes_written = %d, want %d", i, res.GetBytesWritten(), offset)
		}
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("dest content mismatch")
	}
}

func TestRunFileUploadOffsetMismatchAborts(t *testing.T) {
	dest := tmpDest(t, "offset.bin")
	// First chunk writes 10 bytes.
	r1 := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-off", Path: dest, Offset: 0, Data: []byte("0123456789"),
		TotalSize: 20, Overwrite: true,
	})
	if !r1.GetOk() {
		t.Fatalf("chunk 1: %q", r1.GetError())
	}
	// Second chunk claims offset 5 (wrong — should be 10).
	r2 := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-off", Path: dest, Offset: 5, Data: []byte("ABCDE"),
		TotalSize: 20, Overwrite: true,
	})
	if r2.GetOk() {
		t.Fatalf("expected offset mismatch failure")
	}
	if !strings.Contains(r2.GetError(), "offset mismatch") {
		t.Fatalf("error = %q, want offset mismatch", r2.GetError())
	}
	// Temp file must be dropped after the abort.
	if _, err := os.Stat(dest + ".upload-up-off.tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind after abort")
	}
}

func TestRunFileUploadSha256MismatchAborts(t *testing.T) {
	dest := tmpDest(t, "sum.bin")
	data := []byte("hello")
	res := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-sum", Path: dest, Offset: 0, Data: data,
		TotalSize: uint64(len(data)), Sha256: "deadbeef",
		Overwrite: true, Final: true,
	})
	if res.GetOk() {
		t.Fatalf("expected checksum failure")
	}
	if !strings.Contains(res.GetError(), "checksum mismatch") {
		t.Fatalf("error = %q, want checksum mismatch", res.GetError())
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest must not exist after a failed final chunk")
	}
}

func TestRunFileUploadSizeMismatchAborts(t *testing.T) {
	dest := tmpDest(t, "size.bin")
	data := []byte("hello")
	res := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-size", Path: dest, Offset: 0, Data: data,
		TotalSize: 99, // wrong
		Overwrite: true, Final: true,
	})
	if res.GetOk() {
		t.Fatalf("expected size mismatch failure")
	}
	if !strings.Contains(res.GetError(), "size mismatch") {
		t.Fatalf("error = %q, want size mismatch", res.GetError())
	}
}

func TestRunFileUploadRejectsOverwriteFalseWhenDestExists(t *testing.T) {
	dest := tmpDest(t, "exists.bin")
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data := []byte("new")
	sum := sha256Hex(data)
	res := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-ov", Path: dest, Offset: 0, Data: data,
		TotalSize: uint64(len(data)), Sha256: sum,
		Overwrite: false, Final: true,
	})
	if res.GetOk() {
		t.Fatalf("expected destination-exists failure")
	}
	// Original file untouched.
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("dest overwritten: %q", got)
	}
}

func TestRunFileUploadRejectsTraversalAndUNC(t *testing.T) {
	// validateUploadPath rejects UNC paths outright, requires an absolute
	// drive (Windows) / absolute (POSIX) path, and blocks Windows reserved
	// names. (filepath.Clean absorbs `..` on absolute paths, so the `..`
	// segment check is defense-in-depth and not reachable via a normal
	// absolute path — we don't assert it here.)
	cases := map[string]string{
		"\\\\srv\\sh":   "UNC paths are not allowed",
		"//srv/sh":      "UNC paths are not allowed",
	}
	if runtime.GOOS == "windows" {
		cases["CON"] = "absolute drive path required"
		cases["relative\\path"] = "absolute drive path required"
		cases["C:\\CON"] = "reserved name is not allowed"
		cases["C:\\dir\\NUL.txt"] = "reserved name is not allowed"
	} else {
		cases["relative/path"] = "absolute path required"
	}
	for path, want := range cases {
		res := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
			UploadId: "up-bad", Path: path, Data: []byte("x"),
			TotalSize: 1, Overwrite: true, Final: true,
		})
		if res.GetOk() {
			t.Errorf("path %q: expected rejection", path)
			continue
		}
		if !strings.Contains(res.GetError(), want) {
			t.Errorf("path %q: error = %q, want %q", path, res.GetError(), want)
		}
	}
}

func TestRunFileUploadEnforcesChunkAndTotalCaps(t *testing.T) {
	dest := tmpDest(t, "cap.bin")
	// Oversize chunk.
	big := make([]byte, maxUploadChunkBytes+1)
	res := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-chunk", Path: dest, Data: big,
		TotalSize: uint64(len(big)), Overwrite: true, Final: true,
	})
	if res.GetOk() || !strings.Contains(res.GetError(), "chunk exceeds") {
		t.Fatalf("chunk cap: error = %q, ok = %v", res.GetError(), res.GetOk())
	}
	// Oversize total (small chunk, big total).
	res = RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-total", Path: dest, Data: []byte("x"),
		TotalSize: uint64(maxUploadTotalBytes + 1), Overwrite: true, Final: true,
	})
	if res.GetOk() || !strings.Contains(res.GetError(), "upload exceeds") {
		t.Fatalf("total cap: error = %q, ok = %v", res.GetError(), res.GetOk())
	}
}

func TestRunFileUploadEmptyFileCreatesZeroByteFile(t *testing.T) {
	dest := tmpDest(t, "empty.bin")
	sum := sha256Hex(nil)
	res := RunFileUpload(context.Background(), &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-empty", Path: dest, Offset: 0, Data: nil,
		TotalSize: 0, Sha256: sum, Overwrite: true, Final: true,
	})
	if !res.GetOk() {
		t.Fatalf("expected ok, got %q", res.GetError())
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("size = %d, want 0", info.Size())
	}
}

func TestRunFileUploadContextCanceledAborts(t *testing.T) {
	dest := tmpDest(t, "cancel.bin")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := RunFileUpload(ctx, &agentcomposev2.NodeFileUploadRequest{
		UploadId: "up-cancel", Path: dest, Offset: 0, Data: []byte("x"),
		TotalSize: 1, Overwrite: true,
	})
	if res.GetOk() {
		t.Fatalf("expected canceled failure")
	}
	if !strings.Contains(res.GetError(), "canceled") {
		t.Fatalf("error = %q, want canceled", res.GetError())
	}
	if _, err := os.Stat(dest + ".upload-up-cancel.tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind after cancel")
	}
}
