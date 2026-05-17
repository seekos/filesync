package service_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"filesync/internal/model"
	"filesync/internal/repository"
	"filesync/internal/service"
)

func TestSyncRunnerLocalMirror(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("content-a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("b"), 0644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "filesync.db")
	store, err := repository.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	task := model.SyncTask{
		Name:             "t",
		Type:             model.TaskTypeLocal,
		LocalPath:        src,
		DestinationPath:  dst,
		DeleteExtraneous: false,
		Enabled:          false,
		IntervalSeconds:  3600,
		UploadWorkers:    4,
		Status:           model.StatusIdle,
	}
	if err := store.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	tasks := store.ListTasks()
	runner := service.NewSyncRunner(store)
	if err := runner.Run(context.Background(), tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "content-a" {
		t.Fatalf("unexpected a.txt: %q", got)
	}
	got2, err := os.ReadFile(filepath.Join(dst, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != "b" {
		t.Fatalf("unexpected b.txt: %q", got2)
	}
}

func TestSyncRunnerLocalMirrorPrune(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("k"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "stale.txt"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "filesync.db")
	store, err := repository.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	task := model.SyncTask{
		Name:             "t",
		Type:             model.TaskTypeLocal,
		LocalPath:        src,
		DestinationPath:  dst,
		DeleteExtraneous: true,
		Enabled:          false,
		IntervalSeconds:  3600,
		UploadWorkers:    2,
		Status:           model.StatusIdle,
	}
	if err := store.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	tasks := store.ListTasks()
	runner := service.NewSyncRunner(store)
	if err := runner.Run(context.Background(), tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected stale removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Fatal(err)
	}
}
