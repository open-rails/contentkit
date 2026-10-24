package media

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
)

type failingArchiveWriter struct{}

func (failingArchiveWriter) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "owned-scratch", Err: syscall.ENOSPC}
}
func TestArchiveSpoolDiskFailureIsRetryable(t *testing.T) {
	_, _, err := copyArchiveEntry(t.Context(), bytes.NewReader([]byte("valid source")), failingArchiveWriter{}, 1024)
	var invalid *UploadError
	if !errors.Is(err, syscall.ENOSPC) || errors.As(err, &invalid) {
		t.Fatalf("scratch failure was classified as bad ZIP: %v", err)
	}
}

type shortArchiveWriter struct{}

func (shortArchiveWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestArchiveSpoolShortWriteIsRetryable(t *testing.T) {
	_, _, err := copyArchiveEntry(t.Context(), bytes.NewReader([]byte("valid source")), shortArchiveWriter{}, 1024)
	var invalid *UploadError
	if !errors.Is(err, io.ErrShortWrite) || errors.As(err, &invalid) {
		t.Fatalf("short scratch write was classified as bad ZIP: %v", err)
	}
}

type failedArchiveReader struct{}

func (failedArchiveReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestArchiveSpoolMalformedInputIsPermanent(t *testing.T) {
	_, _, err := copyArchiveEntry(t.Context(), failedArchiveReader{}, io.Discard, 1024)
	var invalid *UploadError
	if !errors.As(err, &invalid) || invalid.Code != CodeInvalid {
		t.Fatalf("malformed input was not classified: %v", err)
	}
}
