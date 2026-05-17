package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesync/internal/model"
	"filesync/internal/repository"
)

func TestUploadRefreshesChangedFileSize(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(localPath, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	oldEntry := model.FileEntry{
		Path:    "config.toml",
		Size:    oldInfo.Size(),
		Mode:    uint32(oldInfo.Mode().Perm()),
		ModTime: oldInfo.ModTime().UnixNano(),
	}
	if err := os.WriteFile(localPath, []byte("new-and-longer"), 0600); err != nil {
		t.Fatal(err)
	}

	var gotLength int64
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		gotLength = int64(len(body))
		if r.ContentLength != gotLength {
			t.Errorf("content length mismatch: header=%d body=%d", r.ContentLength, gotLength)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	store, err := repository.NewSQLiteStore(filepath.Join(t.TempDir(), "filesync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runner := NewSyncRunner(store)
	runner.client = &http.Client{Transport: transport}
	task := model.SyncTask{
		ID:              "task",
		Name:            "task",
		LocalPath:       root,
		ReceiverURL:     "http://receiver.invalid",
		DestinationPath: "dest",
	}
	tracker := newUploadProgress(runner, task.ID, oldEntry.Size, "上传文件")
	uploadedEntry, err := runner.upload(context.Background(), task, oldEntry, tracker, 1)
	if err != nil {
		t.Fatal(err)
	}
	if uploadedEntry.Size != gotLength {
		t.Fatalf("expected refreshed entry size %d, got %d", gotLength, uploadedEntry.Size)
	}
}

func TestChunkedUploadAbortsPartialOnFailure(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "large.bin")
	if err := os.WriteFile(localPath, []byte("seed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(localPath, chunkThreshold); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := model.FileEntry{
		Path:    "large.bin",
		Size:    info.Size(),
		Mode:    uint32(info.Mode().Perm()),
		ModTime: info.ModTime().UnixNano(),
	}

	var abortReq model.AbortRequest
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/begin":
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    r,
			}, nil
		case "/api/v1/chunks":
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Status:     "500 Internal Server Error",
				Body:       io.NopCloser(strings.NewReader("disk full")),
				Header:     make(http.Header),
				Request:    r,
			}, nil
		case "/api/v1/abort":
			if err := json.NewDecoder(r.Body).Decode(&abortReq); err != nil {
				t.Fatal(err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    r,
			}, nil
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
			return nil, nil
		}
	})

	store, err := repository.NewSQLiteStore(filepath.Join(t.TempDir(), "filesync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runner := NewSyncRunner(store)
	runner.client = &http.Client{Transport: transport}
	task := model.SyncTask{
		ID:              "task",
		Name:            "task",
		LocalPath:       root,
		ReceiverURL:     "http://receiver.invalid",
		DestinationPath: "dest",
	}
	tracker := newUploadProgress(runner, task.ID, entry.Size, "上传文件")
	if err := runner.uploadChunked(context.Background(), task, entry, tracker, 2); err == nil {
		t.Fatal("expected chunked upload failure")
	}
	if abortReq.Destination != "dest" || abortReq.Path != "large.bin" {
		t.Fatalf("expected abort for dest/large.bin, got %#v", abortReq)
	}
}

func TestStopCancelsRunningTask(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := repository.NewSQLiteStore(filepath.Join(t.TempDir(), "filesync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	task := model.SyncTask{
		ID:              "task",
		Name:            "task",
		LocalPath:       root,
		ReceiverURL:     "http://receiver.invalid",
		DestinationPath: "dest",
		Enabled:         true,
		IntervalSeconds: 3600,
	}
	if err := store.SaveTask(task); err != nil {
		t.Fatal(err)
	}

	requestStarted := make(chan struct{})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(requestStarted)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	runner := NewSyncRunner(store)
	runner.client = &http.Client{Transport: transport}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runner.Run(context.Background(), "task")
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for running request")
	}
	if !runner.Stop("task") {
		t.Fatal("expected Stop to cancel running task")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected canceled run error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stopped task")
	}
	stopped, err := store.GetTask("task")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != model.StatusStopped {
		t.Fatalf("expected stopped status, got %s", stopped.Status)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
