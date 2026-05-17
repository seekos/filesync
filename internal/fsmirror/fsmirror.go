package fsmirror

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filesync/internal/model"
)

// SameModTime compares stored mod time (Unix nano) with file info using the same tolerance as the receiver.
func SameModTime(info os.FileInfo, localUnixNano int64) bool {
	local := time.Unix(0, localUnixNano)
	remote := info.ModTime()
	delta := remote.Sub(local)
	if delta < 0 {
		delta = -delta
	}
	return delta <= time.Second
}

// ReceiverDestinationBase resolves the on-disk root for a receiver destination string.
// Relative destinations are placed under storageRoot. Absolute destinations are
// accepted only when they are inside one of the configured allowedRoots.
func ReceiverDestinationBase(storageRoot, destination string, allowedRoots []string) (string, error) {
	storageRoot = filepath.Clean(storageRoot)
	dest := cleanPath(destination)
	if dest == "" {
		return storageRoot, nil
	}
	if IsAbsPath(dest) {
		base := cleanAbsolute(dest)
		if !isUnderAnyRoot(base, append([]string{storageRoot}, allowedRoots...)) {
			return "", fmt.Errorf("receiver destination is not in allowed_dirs")
		}
		return base, nil
	}
	dest = cleanRelative(dest)
	if invalidRelative(dest) {
		return "", fmt.Errorf("path escapes storage root")
	}
	base := filepath.Join(storageRoot, filepath.FromSlash(dest))
	rootRel, err := filepath.Rel(storageRoot, base)
	if err != nil {
		return "", err
	}
	if rootRel == ".." || strings.HasPrefix(rootRel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes storage root")
	}
	return base, nil
}

func cleanAbsolute(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 3 && isASCIILetter(value[0]) && value[1] == ':' && (value[2] == '/' || value[2] == '\\') {
		value = strings.ReplaceAll(value, "\\", "/")
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.FromSlash(value))
}

func isUnderAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		root = cleanAbsolute(root)
		if pathWithinRoot(root, path) {
			return true
		}
	}
	return false
}

func pathWithinRoot(root, path string) bool {
	if path == root || isSubpath(root, path) {
		return true
	}
	rootSlash := strings.TrimRight(filepath.ToSlash(root), "/")
	pathSlash := filepath.ToSlash(path)
	if hasWindowsDrive(rootSlash) || hasWindowsDrive(pathSlash) {
		rootSlash = strings.ToLower(rootSlash)
		pathSlash = strings.ToLower(pathSlash)
	}
	return pathSlash == rootSlash || strings.HasPrefix(pathSlash, rootSlash+"/")
}

func hasWindowsDrive(value string) bool {
	return len(value) >= 2 && isASCIILetter(value[0]) && value[1] == ':'
}

// LocalMirrorRoot validates and cleans an absolute local mirror root path.
func LocalMirrorRoot(destination string) (string, error) {
	dest := strings.TrimSpace(destination)
	if dest == "" {
		return "", fmt.Errorf("destination path is required")
	}
	dest = filepath.Clean(filepath.FromSlash(dest))
	if !IsAbsPath(dest) {
		return "", fmt.Errorf("local destination must be an absolute path")
	}
	return dest, nil
}

// PathsOverlap returns true if either path is equal to or nested inside the other (same-volume / OS semantics).
func PathsOverlap(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	return isSubpath(a, b) || isSubpath(b, a)
}

func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

// SafeJoin joins an absolute base with a relative slash-separated path from manifests.
func SafeJoin(base, relPath string) (string, error) {
	base = filepath.Clean(base)
	rel := cleanRelative(relPath)
	if invalidRelative(rel) || IsAbsPath(rel) {
		return "", fmt.Errorf("path escapes storage root")
	}
	target := filepath.Join(base, filepath.FromSlash(rel))
	targetRel, err := filepath.Rel(base, target)
	if err != nil {
		return "", err
	}
	if targetRel == ".." || strings.HasPrefix(targetRel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes storage root")
	}
	return target, nil
}

// NeededRelativePaths returns manifest paths that are missing or differ at the destination root.
func NeededRelativePaths(destRoot string, files []model.FileEntry) ([]string, error) {
	destRoot = filepath.Clean(destRoot)
	var needed []string
	for _, file := range files {
		target, err := SafeJoin(destRoot, file.Path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(target)
		if errors.Is(err, os.ErrNotExist) {
			needed = append(needed, file.Path)
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Size() != file.Size || !SameModTime(info, file.ModTime) {
			needed = append(needed, file.Path)
		}
	}
	return needed, nil
}

// PruneKeepingRelative removes files under root that are not listed in keep (relative slash paths).
func PruneKeepingRelative(root string, keepRelPaths []string) error {
	root = filepath.Clean(root)
	keep := make(map[string]struct{}, len(keepRelPaths))
	for _, file := range keepRelPaths {
		keep[filepath.ToSlash(filepath.Clean(file))] = struct{}{}
	}
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, ok := keep[filepath.ToSlash(rel)]; !ok {
			return os.Remove(path)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
	return nil
}

func cleanPath(value string) string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	value = filepath.ToSlash(filepath.Clean(value))
	if value == "." {
		return ""
	}
	return value
}

func cleanRelative(value string) string {
	value = cleanPath(value)
	value = strings.TrimLeft(value, "/")
	if value == "." {
		return ""
	}
	return value
}

func invalidRelative(value string) bool {
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "../") || value == ".." || strings.Contains(value, "/../") {
		return true
	}
	return strings.Contains(value, "\x00")
}

// IsAbsPath reports whether value is an absolute path, including Windows drive paths.
func IsAbsPath(value string) bool {
	if filepath.IsAbs(value) {
		return true
	}
	if len(value) >= 3 && isASCIILetter(value[0]) && value[1] == ':' && (value[2] == '/' || value[2] == '\\') {
		return true
	}
	return strings.HasPrefix(value, "//")
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}
