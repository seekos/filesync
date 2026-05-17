package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesync/internal/fsmirror"
	"filesync/internal/model"
)

func TestReceiverManifestReportsMissingFiles(t *testing.T) {
	mux := receiverMux(t, "secret")
	payload, _ := json.Marshal(model.ManifestRequest{
		Destination: "server-a/app",
		Files: []model.FileEntry{{
			Path:    "data.txt",
			Size:    5,
			Mode:    0644,
			ModTime: time.Now().UnixNano(),
		}},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/manifest", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "data.txt") {
		t.Fatalf("expected missing file in manifest response")
	}
}

func TestReceiverUploadStoresFile(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	NewReceiverController(root, "secret").Register(mux)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/files?destination=server-a/app&path=data.txt&mode=420&mtime=1710000000000000000", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "server-a", "app", "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected uploaded data, got %q", string(data))
	}
}

func TestReceiverChunkUploadStoresFile(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	NewReceiverController(root, "secret").Register(mux)

	chunks := []struct {
		offset string
		body   string
	}{
		{offset: "5", body: " world"},
		{offset: "0", body: "hello"},
	}
	for _, chunk := range chunks {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/chunks?destination=server-a/app&path=data.txt&offset="+chunk.offset, strings.NewReader(chunk.body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected chunk status 200, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	payload, _ := json.Marshal(model.CompleteRequest{
		Destination: "server-a/app",
		Path:        "data.txt",
		Mode:        0644,
		ModTime:     time.Now().UnixNano(),
		Size:        11,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/complete", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected complete status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(filepath.Join(root, "server-a", "app", "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("expected completed chunk data, got %q", string(data))
	}
}

func TestReceiverChunkUploadRemovesPartialOnCopyError(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	NewReceiverController(root, "secret").Register(mux)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/chunks?destination=server-a/app&path=data.txt&offset=0", &failingBody{})
	req.ContentLength = 12
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", rec.Code)
	}
	tmp := filepath.Join(root, "server-a", "app", "data.txt.uploading")
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("expected partial upload to be removed, stat error: %v", err)
	}
}

func TestReceiverAbortRemovesPartialUpload(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	NewReceiverController(root, "secret").Register(mux)
	tmp := filepath.Join(root, "server-a", "app", "data.txt.uploading")
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(model.AbortRequest{
		Destination: "server-a/app",
		Path:        "data.txt",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/abort", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("expected partial upload to be removed, stat error: %v", err)
	}
}

func TestReceiverRejectsEscapedPath(t *testing.T) {
	mux := receiverMux(t, "secret")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/files?destination=server-a&path=../bad.txt", strings.NewReader("bad"))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
}

func TestReceiverRejectsUnallowedAbsoluteDestination(t *testing.T) {
	base, err := fsmirror.ReceiverDestinationBase(`C:\Users\amov\.filesync\storage`, `E:\filesync\mac\rust-web-learning`, nil)
	if err == nil {
		t.Fatalf("expected absolute destination to be rejected, got base %q", base)
	}
}

func TestReceiverAllowsConfiguredAbsoluteDestination(t *testing.T) {
	base, err := fsmirror.ReceiverDestinationBase(`C:\Users\amov\.filesync\storage`, `E:\filesync\mac\rust-web-learning`, []string{`E:\filesync`})
	if err != nil {
		t.Fatal(err)
	}
	if base == "" {
		t.Fatal("expected resolved absolute destination")
	}
}

func receiverMux(t *testing.T, token string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	NewReceiverController(t.TempDir(), token).Register(mux)
	return mux
}

type failingBody struct {
	sent bool
}

func (b *failingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("forced read failure")
}
