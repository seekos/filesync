package fsmirror_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesync/internal/fsmirror"
	"filesync/internal/model"
)

func TestNeededRelativePaths(t *testing.T) {
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "a"), 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dest, "a", "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	files := []model.FileEntry{
		{Path: "a/f.txt", Size: 5, Mode: 0644, ModTime: info.ModTime().UnixNano()},
		{Path: "b/x.bin", Size: 1, Mode: 0644, ModTime: time.Now().UnixNano()},
	}
	needed, err := fsmirror.NeededRelativePaths(dest, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(needed) != 1 || needed[0] != "b/x.bin" {
		t.Fatalf("expected only b/x.bin needed, got %#v", needed)
	}
	if err := os.MkdirAll(filepath.Join(dest, "b"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "b", "x.bin"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	needed2, err := fsmirror.NeededRelativePaths(dest, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(needed2) != 0 {
		t.Fatalf("expected no needed files, got %#v", needed2)
	}
}

func TestPruneKeepingRelative(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"), "a")
	mustWrite(t, filepath.Join(root, "drop.txt"), "b")
	if err := fsmirror.PruneKeepingRelative(root, []string{"keep.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "drop.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected drop.txt removed: %v", err)
	}
}

func TestPathsOverlap(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "nested")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if !fsmirror.PathsOverlap(base, nested) {
		t.Fatal("expected overlap")
	}
	if !fsmirror.PathsOverlap(nested, base) {
		t.Fatal("expected overlap symmetric")
	}
	if fsmirror.PathsOverlap(base, other) {
		t.Fatal("expected no overlap")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
