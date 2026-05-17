package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"filesync/internal/fsmirror"
	"filesync/internal/model"
)

func (r *SyncRunner) syncLocal(ctx context.Context, task model.SyncTask) error {
	if task.LocalPath == "" || task.DestinationPath == "" {
		return fmt.Errorf("local path and destination path are required")
	}
	destRoot, err := fsmirror.LocalMirrorRoot(task.DestinationPath)
	if err != nil {
		return err
	}
	localRoot := filepath.Clean(task.LocalPath)
	if _, err := os.Stat(localRoot); err != nil {
		return fmt.Errorf("local path: %w", err)
	}
	if fsmirror.PathsOverlap(localRoot, destRoot) {
		return fmt.Errorf("local_path must not overlap destination_path")
	}

	r.updateProgress(task.ID, func(t *model.SyncTask, now time.Time) {
		t.CurrentStep = "扫描文件"
		t.CurrentFile = ""
		t.LastProgressAt = &now
	})
	files, err := scanFiles(ctx, localRoot, task.Excludes)
	if err != nil {
		return err
	}
	r.updateProgress(task.ID, func(t *model.SyncTask, now time.Time) {
		t.CurrentStep = "比对目标目录"
		t.ScannedFiles = len(files)
		t.LastProgressAt = &now
	})
	workers := normalizedWorkers(task.UploadWorkers)
	needed, err := neededLocalPaths(ctx, destRoot, files, workers)
	if err != nil {
		return err
	}
	byPath := make(map[string]model.FileEntry, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	neededBytes := sumNeededBytes(needed, byPath)
	r.updateProgress(task.ID, func(t *model.SyncTask, now time.Time) {
		t.CurrentStep = "复制文件"
		t.NeededFiles = len(needed)
		t.UploadedFiles = 0
		t.NeededBytes = neededBytes
		t.UploadedBytes = 0
		t.UploadSpeedBps = 0
		t.ETASeconds = 0
		t.CurrentFile = ""
		t.CurrentFileSize = 0
		t.CurrentFileBytes = 0
		t.LastProgressAt = &now
	})
	tracker := newUploadProgress(r, task.ID, neededBytes, "复制文件")
	if err := r.copyNeeded(ctx, task, needed, byPath, destRoot, tracker); err != nil {
		return err
	}
	if task.DeleteExtraneous {
		r.updateProgress(task.ID, func(t *model.SyncTask, now time.Time) {
			t.CurrentStep = "清理多余文件"
			t.CurrentFile = ""
			t.CurrentFileSize = 0
			t.CurrentFileBytes = 0
			t.LastProgressAt = &now
		})
		keep := make([]string, 0, len(files))
		for _, file := range files {
			keep = append(keep, file.Path)
		}
		if err := fsmirror.PruneKeepingRelative(destRoot, keep); err != nil {
			return err
		}
	}
	log.Printf("task=%s local mirror: synced %d changed files from %d scanned files", task.Name, len(needed), len(files))
	return nil
}

func (r *SyncRunner) copyNeeded(ctx context.Context, task model.SyncTask, needed []string, byPath map[string]model.FileEntry, destRoot string, tracker *uploadProgress) error {
	workers := normalizedWorkers(task.UploadWorkers)
	fileWorkers := workers
	if onlyLargeFiles(needed, byPath) && fileWorkers > 4 {
		fileWorkers = 4
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan model.FileEntry)
	errCh := make(chan error, 1)
	dirCache := &sync.Map{}
	var wg sync.WaitGroup
	for i := 0; i < fileWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range jobs {
				tracker.startFile(entry)
				if err := r.copyLocal(ctx, task, entry, destRoot, tracker, dirCache); err != nil {
					tracker.failFile(entry)
					select {
					case errCh <- err:
						cancel()
					default:
					}
					return
				}
				tracker.finishFile(entry)
			}
		}()
	}
sendLoop:
	for _, rel := range needed {
		entry, ok := byPath[rel]
		if !ok {
			continue
		}
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- entry:
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		tracker.flush()
		return nil
	}
}

func (r *SyncRunner) copyLocal(ctx context.Context, task model.SyncTask, entry model.FileEntry, destRoot string, tracker *uploadProgress, dirCache *sync.Map) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			tracker.startFile(entry)
		}
		if err := r.copyLocalOnce(ctx, task, entry, destRoot, tracker, dirCache); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return err
			}
			log.Printf("local copy retry task=%s file=%s attempt=%d error=%v", task.Name, entry.Path, attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		return nil
	}
	return lastErr
}

func (r *SyncRunner) copyLocalOnce(ctx context.Context, task model.SyncTask, entry model.FileEntry, destRoot string, tracker *uploadProgress, dirCache *sync.Map) error {
	srcPath := filepath.Join(filepath.Clean(task.LocalPath), filepath.FromSlash(entry.Path))
	dstPath, err := fsmirror.SafeJoin(destRoot, entry.Path)
	if err != nil {
		return err
	}
	dstDir := filepath.Dir(dstPath)
	if _, ok := dirCache.Load(dstDir); !ok {
		if err := os.MkdirAll(dstDir, 0755); err != nil {
			return err
		}
		dirCache.Store(dstDir, struct{}{})
	}
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	tmp := dstPath + ".uploading"
	mode := os.FileMode(entry.Mode) & 0777
	if mode == 0 {
		mode = 0644
	}
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if entry.Size >= chunkThreshold {
		if err := out.Truncate(entry.Size); err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	reader := &progressReader{
		reader:  io.LimitReader(srcFile, entry.Size),
		entry:   entry,
		tracker: tracker,
	}
	_, copyErr := copyBufferContext(ctx, out, reader)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	modTime := time.Unix(0, entry.ModTime)
	if entry.ModTime <= 0 {
		modTime = time.Now()
	}
	_ = os.Chmod(tmp, mode)
	_ = os.Chtimes(tmp, modTime, modTime)
	if err := verifyStableFile(srcPath, entry); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func normalizedWorkers(workers int) int {
	if workers <= 0 {
		return defaultUploadWorkers
	}
	if workers > maxUploadWorkers {
		return maxUploadWorkers
	}
	return workers
}

func onlyLargeFiles(needed []string, byPath map[string]model.FileEntry) bool {
	if len(needed) == 0 {
		return false
	}
	for _, rel := range needed {
		entry, ok := byPath[rel]
		if !ok || entry.Size < chunkThreshold {
			return false
		}
	}
	return true
}

func neededLocalPaths(ctx context.Context, destRoot string, files []model.FileEntry, workers int) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(files) {
		workers = len(files)
	}

	type result struct {
		index  int
		needed bool
	}
	jobs := make(chan int)
	results := make(chan result, len(files))
	errCh := make(chan error, 1)
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				needed, err := localFileNeeded(destRoot, files[index])
				if err != nil {
					select {
					case errCh <- err:
						cancel()
					default:
					}
					return
				}
				select {
				case results <- result{index: index, needed: needed}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

sentLoop:
	for index := range files {
		select {
		case <-ctx.Done():
			break sentLoop
		case jobs <- index:
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	select {
	case err := <-errCh:
		return nil, err
	default:
	}
	if err := parentCtx.Err(); err != nil {
		return nil, err
	}

	flags := make([]bool, len(files))
	for res := range results {
		flags[res.index] = res.needed
	}
	needed := make([]string, 0)
	for index, file := range files {
		if flags[index] {
			needed = append(needed, file.Path)
		}
	}
	return needed, nil
}

func localFileNeeded(destRoot string, file model.FileEntry) (bool, error) {
	target, err := fsmirror.SafeJoin(destRoot, file.Path)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return info.Size() != file.Size || !fsmirror.SameModTime(info, file.ModTime), nil
}

func copyBufferContext(ctx context.Context, w io.Writer, r io.Reader) (written int64, err error) {
	buf := getServiceIOBuffer()
	defer putServiceIOBuffer(buf)
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = fmt.Errorf("invalid write result")
				}
			}
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}
