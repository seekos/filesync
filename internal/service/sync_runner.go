package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"filesync/internal/model"
	"filesync/internal/repository"
)

const (
	defaultUploadWorkers = 32
	maxUploadWorkers     = 128
	chunkThreshold       = 64 * 1024 * 1024
	chunkSize            = 64 * 1024 * 1024
	ioBufferSize         = 1024 * 1024
	progressInterval     = 1500 * time.Millisecond
)

type SyncRunner struct {
	store   repository.TaskStore
	client  *http.Client
	running sync.Map
	cancels sync.Map
}

func NewSyncRunner(store repository.TaskStore) *SyncRunner {
	return &SyncRunner{
		store: store,
		client: &http.Client{
			Timeout:   0,
			Transport: fastTransport(),
		},
	}
}

func (r *SyncRunner) Run(ctx context.Context, taskID string) error {
	task, err := r.store.GetTask(taskID)
	if err != nil {
		return err
	}
	if _, loaded := r.running.LoadOrStore(taskID, struct{}{}); loaded {
		return fmt.Errorf("task %s is already running", task.Name)
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.cancels.Store(taskID, cancel)
	defer func() {
		cancel()
		r.cancels.Delete(taskID)
		r.running.Delete(taskID)
	}()

	now := time.Now()
	_ = r.store.UpdateTask(taskID, func(t *model.SyncTask) {
		t.Status = model.StatusRunning
		t.LastRunAt = &now
		t.LastError = ""
		t.CurrentStep = "准备同步"
		t.CurrentFile = ""
		t.ScannedFiles = 0
		t.NeededFiles = 0
		t.UploadedFiles = 0
		t.NeededBytes = 0
		t.UploadedBytes = 0
		t.UploadSpeedBps = 0
		t.ETASeconds = 0
		t.CurrentFileSize = 0
		t.CurrentFileBytes = 0
		t.LastProgressAt = &now
	})

	err = r.sync(runCtx, task)
	finished := time.Now()
	_ = r.store.UpdateTask(taskID, func(t *model.SyncTask) {
		if err != nil {
			if errors.Is(err, context.Canceled) {
				t.Status = model.StatusStopped
				t.LastError = ""
				t.CurrentStep = "已停止"
				t.CurrentFile = ""
				t.UploadSpeedBps = 0
				t.ETASeconds = 0
				t.CurrentFileSize = 0
				t.CurrentFileBytes = 0
				t.LastProgressAt = &finished
				return
			}
			t.Status = model.StatusFailed
			t.LastError = err.Error()
			t.CurrentStep = "同步失败"
			t.UploadSpeedBps = 0
			t.ETASeconds = 0
			t.LastProgressAt = &finished
			return
		}
		t.Status = model.StatusSuccess
		t.LastSuccessAt = &finished
		t.LastError = ""
		t.CurrentStep = "同步完成"
		t.CurrentFile = ""
		t.UploadSpeedBps = 0
		t.ETASeconds = 0
		t.CurrentFileSize = 0
		t.CurrentFileBytes = 0
		t.LastProgressAt = &finished
	})
	return err
}

func (r *SyncRunner) Stop(taskID string) bool {
	value, ok := r.cancels.Load(taskID)
	if !ok {
		return false
	}
	cancel, ok := value.(context.CancelFunc)
	if !ok {
		return false
	}
	cancel()
	return true
}

func (r *SyncRunner) sync(ctx context.Context, task model.SyncTask) error {
	if task.IsLocal() {
		return r.syncLocal(ctx, task)
	}
	if task.LocalPath == "" || task.ReceiverURL == "" || task.DestinationPath == "" {
		return fmt.Errorf("local path, receiver URL and destination path are required")
	}
	if _, err := os.Stat(task.LocalPath); err != nil {
		return fmt.Errorf("local path: %w", err)
	}

	r.updateProgress(task.ID, func(t *model.SyncTask, now time.Time) {
		t.CurrentStep = "扫描文件"
		t.CurrentFile = ""
		t.LastProgressAt = &now
	})
	files, err := scanFiles(ctx, task.LocalPath, task.Excludes)
	if err != nil {
		return err
	}
	r.updateProgress(task.ID, func(t *model.SyncTask, now time.Time) {
		t.CurrentStep = "比对接收端"
		t.ScannedFiles = len(files)
		t.LastProgressAt = &now
	})
	manifest := model.ManifestRequest{
		Destination:      task.DestinationPath,
		Files:            files,
		DeleteExtraneous: task.DeleteExtraneous,
	}
	needed, err := r.askNeeded(ctx, task, manifest)
	if err != nil {
		return err
	}
	byPath := make(map[string]model.FileEntry, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	neededBytes := sumNeededBytes(needed, byPath)
	r.updateProgress(task.ID, func(t *model.SyncTask, now time.Time) {
		t.CurrentStep = "上传文件"
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
	if err := r.uploadNeeded(ctx, task, needed, byPath); err != nil {
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
		if err := r.prune(ctx, task, files); err != nil {
			return err
		}
	}
	log.Printf("task=%s synced %d changed files from %d scanned files", task.Name, len(needed), len(files))
	return nil
}

func (r *SyncRunner) uploadNeeded(ctx context.Context, task model.SyncTask, needed []string, byPath map[string]model.FileEntry) error {
	workers := task.UploadWorkers
	if workers <= 0 {
		workers = defaultUploadWorkers
	}
	if workers > maxUploadWorkers {
		workers = maxUploadWorkers
	}
	fileWorkers := workers
	if hasLargeFile(needed, byPath) && fileWorkers > 4 {
		fileWorkers = 4
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tracker := newUploadProgress(r, task.ID, sumNeededBytes(needed, byPath), "上传文件")
	jobs := make(chan model.FileEntry)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < fileWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range jobs {
				tracker.startFile(entry)
				uploadedEntry, err := r.upload(ctx, task, entry, tracker, workers)
				if err != nil {
					tracker.failFile(entry)
					select {
					case errCh <- err:
						cancel()
					default:
					}
					return
				}
				tracker.finishFile(uploadedEntry)
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

func sumNeededBytes(needed []string, byPath map[string]model.FileEntry) int64 {
	var total int64
	for _, rel := range needed {
		if entry, ok := byPath[rel]; ok {
			total += entry.Size
		}
	}
	return total
}

func hasLargeFile(needed []string, byPath map[string]model.FileEntry) bool {
	for _, rel := range needed {
		if entry, ok := byPath[rel]; ok && entry.Size >= chunkThreshold {
			return true
		}
	}
	return false
}

func fastTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
		WriteBufferSize:       ioBufferSize,
		ReadBufferSize:        ioBufferSize,
	}
}

func (r *SyncRunner) updateProgress(taskID string, fn func(*model.SyncTask, time.Time)) {
	now := time.Now()
	_ = r.store.UpdateTask(taskID, func(t *model.SyncTask) {
		fn(t, now)
	})
}

type uploadProgress struct {
	runner         *SyncRunner
	taskID         string
	totalBytes     int64
	startedAt      time.Time
	lastUpdateAt   time.Time
	completedBytes int64
	completedFiles int
	inFlight       map[string]int64
	currentEntry   model.FileEntry
	currentBytes   int64
	transferStep   string
	mu             sync.Mutex
}

type progressReader struct {
	reader  io.Reader
	entry   model.FileEntry
	tracker *uploadProgress
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.tracker.add(r.entry, int64(n))
	}
	return n, err
}

func (r *progressReader) WriteTo(w io.Writer) (int64, error) {
	buf := getServiceIOBuffer()
	defer putServiceIOBuffer(buf)
	var total int64
	for {
		n, readErr := r.reader.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			if written > 0 {
				total += int64(written)
				r.tracker.add(r.entry, int64(written))
			}
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
	}
}

func newUploadProgress(runner *SyncRunner, taskID string, totalBytes int64, transferStep string) *uploadProgress {
	now := time.Now()
	return &uploadProgress{
		runner:       runner,
		taskID:       taskID,
		totalBytes:   totalBytes,
		startedAt:    now,
		lastUpdateAt: now,
		inFlight:     make(map[string]int64),
		transferStep: transferStep,
	}
}

func (p *uploadProgress) transferStepLabel() string {
	if p.transferStep != "" {
		return p.transferStep
	}
	return "上传文件"
}

func (p *uploadProgress) startFile(entry model.FileEntry) {
	p.mu.Lock()
	p.inFlight[entry.Path] = 0
	p.currentEntry = entry
	p.currentBytes = 0
	p.mu.Unlock()
	p.publish(entry, 0, false)
}

func (p *uploadProgress) add(entry model.FileEntry, n int64) {
	p.mu.Lock()
	p.inFlight[entry.Path] += n
	currentBytes := p.inFlight[entry.Path]
	p.currentEntry = entry
	p.currentBytes = currentBytes
	shouldPublish := time.Since(p.lastUpdateAt) >= progressInterval
	p.mu.Unlock()
	if shouldPublish {
		p.publish(entry, currentBytes, false)
	}
}

func (p *uploadProgress) finishFile(entry model.FileEntry) {
	p.mu.Lock()
	delete(p.inFlight, entry.Path)
	p.completedBytes += entry.Size
	p.completedFiles++
	p.currentEntry = entry
	p.currentBytes = entry.Size
	shouldPublish := time.Since(p.lastUpdateAt) >= progressInterval
	p.mu.Unlock()
	if shouldPublish {
		p.publish(entry, entry.Size, false)
	}
}

func (p *uploadProgress) failFile(entry model.FileEntry) {
	p.mu.Lock()
	delete(p.inFlight, entry.Path)
	p.mu.Unlock()
	p.publish(entry, 0, true)
}

func (p *uploadProgress) publish(entry model.FileEntry, currentBytes int64, force bool) {
	p.mu.Lock()
	if !force && time.Since(p.lastUpdateAt) < progressInterval {
		p.mu.Unlock()
		return
	}
	p.lastUpdateAt = time.Now()
	p.mu.Unlock()
	p.runner.updateProgress(p.taskID, func(t *model.SyncTask, now time.Time) {
		p.applyTotals(t, now)
		t.CurrentStep = p.transferStepLabel()
		t.CurrentFile = entry.Path
		t.CurrentFileSize = entry.Size
		t.CurrentFileBytes = currentBytes
		t.LastProgressAt = &now
	})
}

func (p *uploadProgress) flush() {
	p.mu.Lock()
	entry := p.currentEntry
	currentBytes := p.currentBytes
	p.mu.Unlock()
	p.runner.updateProgress(p.taskID, func(t *model.SyncTask, now time.Time) {
		p.applyTotals(t, now)
		t.CurrentStep = p.transferStepLabel()
		t.CurrentFile = entry.Path
		t.CurrentFileSize = entry.Size
		t.CurrentFileBytes = currentBytes
		t.LastProgressAt = &now
	})
}

func (p *uploadProgress) applyTotals(t *model.SyncTask, now time.Time) {
	p.mu.Lock()
	sent := p.completedBytes
	for _, bytes := range p.inFlight {
		sent += bytes
	}
	completedFiles := p.completedFiles
	p.mu.Unlock()
	t.NeededBytes = p.totalBytes
	t.UploadedBytes = sent
	t.UploadedFiles = completedFiles
	elapsed := now.Sub(p.startedAt).Seconds()
	if elapsed > 0 {
		t.UploadSpeedBps = int64(float64(sent) / elapsed)
	}
	if t.UploadSpeedBps > 0 && p.totalBytes > sent {
		t.ETASeconds = (p.totalBytes - sent) / t.UploadSpeedBps
	} else {
		t.ETASeconds = 0
	}
}

func scanFiles(ctx context.Context, root string, excludes []string) ([]model.FileEntry, error) {
	root = filepath.Clean(root)
	var files []model.FileEntry
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if excluded(rel, excludes) {
			return nil
		}
		files = append(files, model.FileEntry{
			Path:    rel,
			Size:    info.Size(),
			Mode:    uint32(info.Mode().Perm()),
			ModTime: info.ModTime().UnixNano(),
		})
		return nil
	})
	return files, err
}

func excluded(rel string, patterns []string) bool {
	base := filepath.Base(rel)
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(filepath.ToSlash(pattern))
		if pattern == "" {
			continue
		}
		if ok, _ := filepath.Match(pattern, rel); ok {
			return true
		}
		if ok, _ := filepath.Match(pattern, base); ok {
			return true
		}
		if strings.HasSuffix(pattern, "/") && strings.HasPrefix(rel, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
	}
	return false
}

func (r *SyncRunner) askNeeded(ctx context.Context, task model.SyncTask, manifest model.ManifestRequest) ([]string, error) {
	var resp model.ManifestResponse
	if err := r.postJSON(ctx, task, "/api/v1/manifest", manifest, &resp); err != nil {
		return nil, err
	}
	return resp.Needed, nil
}

func (r *SyncRunner) prune(ctx context.Context, task model.SyncTask, files []model.FileEntry) error {
	keep := make([]string, 0, len(files))
	for _, file := range files {
		keep = append(keep, file.Path)
	}
	return r.postJSON(ctx, task, "/api/v1/prune", model.PruneRequest{
		Destination: task.DestinationPath,
		Files:       keep,
	}, nil)
}

func (r *SyncRunner) postJSON(ctx context.Context, task model.SyncTask, apiPath string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint, err := joinURL(task.ReceiverURL, apiPath)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	authorize(req, task.AuthToken)

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("receiver %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *SyncRunner) upload(ctx context.Context, task model.SyncTask, entry model.FileEntry, tracker *uploadProgress, workers int) (model.FileEntry, error) {
	var lastErr error
	uploadedEntry := entry
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			tracker.startFile(entry)
		}
		freshEntry, err := refreshEntryFromDisk(filepath.Join(filepath.Clean(task.LocalPath), filepath.FromSlash(entry.Path)), entry)
		if err != nil {
			return uploadedEntry, err
		}
		uploadedEntry = freshEntry
		if err := r.uploadOnce(ctx, task, freshEntry, tracker, workers); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return uploadedEntry, err
			}
			log.Printf("upload retry task=%s file=%s attempt=%d error=%v", task.Name, entry.Path, attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		return uploadedEntry, nil
	}
	return uploadedEntry, lastErr
}

func (r *SyncRunner) uploadOnce(ctx context.Context, task model.SyncTask, entry model.FileEntry, tracker *uploadProgress, workers int) error {
	if entry.Size >= chunkThreshold && workers > 1 {
		return r.uploadChunked(ctx, task, entry, tracker, workers)
	}
	return r.uploadWhole(ctx, task, entry, tracker)
}

func (r *SyncRunner) uploadWhole(ctx context.Context, task model.SyncTask, entry model.FileEntry, tracker *uploadProgress) error {
	localPath := filepath.Join(filepath.Clean(task.LocalPath), filepath.FromSlash(entry.Path))
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	entry = entryFromInfo(entry.Path, info)

	endpoint, err := joinURL(task.ReceiverURL, "/api/v1/files")
	if err != nil {
		return err
	}
	values := url.Values{}
	values.Set("destination", task.DestinationPath)
	values.Set("path", entry.Path)
	values.Set("mode", strconv.FormatUint(uint64(entry.Mode), 10))
	values.Set("mtime", strconv.FormatInt(entry.ModTime, 10))
	endpoint += "?" + values.Encode()

	reader := &progressReader{
		reader:  io.LimitReader(file, entry.Size),
		entry:   entry,
		tracker: tracker,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, reader)
	if err != nil {
		return err
	}
	req.ContentLength = entry.Size
	req.Header.Set("Content-Type", mime.TypeByExtension(filepath.Ext(entry.Path)))
	authorize(req, task.AuthToken)

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upload %s failed: %s %s", entry.Path, resp.Status, strings.TrimSpace(string(data)))
	}
	return verifyStableFile(localPath, entry)
}

func (r *SyncRunner) uploadChunked(ctx context.Context, task model.SyncTask, entry model.FileEntry, tracker *uploadProgress, workers int) error {
	localPath := filepath.Join(filepath.Clean(task.LocalPath), filepath.FromSlash(entry.Path))
	var err error
	entry, err = refreshEntryFromDisk(localPath, entry)
	if err != nil {
		return err
	}
	if err := r.beginChunkedUpload(ctx, task, entry); err != nil {
		return err
	}
	chunks := splitChunks(entry.Size)
	chunkWorkers := workers
	if chunkWorkers > len(chunks) {
		chunkWorkers = len(chunks)
	}
	if chunkWorkers < 1 {
		chunkWorkers = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan fileChunk)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < chunkWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range jobs {
				if err := r.uploadChunk(ctx, task, entry, localPath, chunk, tracker); err != nil {
					select {
					case errCh <- err:
						cancel()
					default:
					}
					return
				}
			}
		}()
	}
sendLoop:
	for _, chunk := range chunks {
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- chunk:
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		r.abortChunkedUpload(task, entry)
		return err
	default:
		if err := verifyStableFile(localPath, entry); err != nil {
			r.abortChunkedUpload(task, entry)
			return err
		}
		if err := r.completeChunkedUpload(ctx, task, entry); err != nil {
			r.abortChunkedUpload(task, entry)
			return err
		}
		return nil
	}
}

type fileChunk struct {
	offset int64
	size   int64
}

func splitChunks(size int64) []fileChunk {
	var chunks []fileChunk
	for offset := int64(0); offset < size; offset += chunkSize {
		n := int64(chunkSize)
		if offset+n > size {
			n = size - offset
		}
		chunks = append(chunks, fileChunk{offset: offset, size: n})
	}
	return chunks
}

func (r *SyncRunner) uploadChunk(ctx context.Context, task model.SyncTask, entry model.FileEntry, localPath string, chunk fileChunk, tracker *uploadProgress) error {
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(chunk.offset, io.SeekStart); err != nil {
		return err
	}
	limited := io.LimitReader(file, chunk.size)
	reader := &progressReader{
		reader:  limited,
		entry:   entry,
		tracker: tracker,
	}

	endpoint, err := joinURL(task.ReceiverURL, "/api/v1/chunks")
	if err != nil {
		return err
	}
	values := url.Values{}
	values.Set("destination", task.DestinationPath)
	values.Set("path", entry.Path)
	values.Set("offset", strconv.FormatInt(chunk.offset, 10))
	endpoint += "?" + values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, reader)
	if err != nil {
		return err
	}
	req.ContentLength = chunk.size
	req.Header.Set("Content-Type", "application/octet-stream")
	authorize(req, task.AuthToken)

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("chunk upload %s@%d failed: %s %s", entry.Path, chunk.offset, resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func refreshEntryFromDisk(localPath string, entry model.FileEntry) (model.FileEntry, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return entry, err
	}
	if !info.Mode().IsRegular() {
		return entry, fmt.Errorf("local path is not a regular file: %s", localPath)
	}
	return entryFromInfo(entry.Path, info), nil
}

func entryFromInfo(relPath string, info os.FileInfo) model.FileEntry {
	return model.FileEntry{
		Path:    relPath,
		Size:    info.Size(),
		Mode:    uint32(info.Mode().Perm()),
		ModTime: info.ModTime().UnixNano(),
	}
}

func verifyStableFile(localPath string, entry model.FileEntry) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	if info.Size() != entry.Size || info.ModTime().UnixNano() != entry.ModTime {
		return fmt.Errorf("file changed during transfer: %s", entry.Path)
	}
	return nil
}

func (r *SyncRunner) completeChunkedUpload(ctx context.Context, task model.SyncTask, entry model.FileEntry) error {
	return r.postJSON(ctx, task, "/api/v1/complete", model.CompleteRequest{
		Destination: task.DestinationPath,
		Path:        entry.Path,
		Mode:        entry.Mode,
		ModTime:     entry.ModTime,
		Size:        entry.Size,
	}, nil)
}

func (r *SyncRunner) beginChunkedUpload(ctx context.Context, task model.SyncTask, entry model.FileEntry) error {
	return r.postJSON(ctx, task, "/api/v1/begin", model.BeginRequest{
		Destination: task.DestinationPath,
		Path:        entry.Path,
		Mode:        entry.Mode,
		ModTime:     entry.ModTime,
		Size:        entry.Size,
	}, nil)
}

func (r *SyncRunner) abortChunkedUpload(task model.SyncTask, entry model.FileEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.postJSON(ctx, task, "/api/v1/abort", model.AbortRequest{
		Destination: task.DestinationPath,
		Path:        entry.Path,
	}, nil)
	if err != nil {
		log.Printf("abort upload cleanup failed task=%s file=%s error=%v", task.Name, entry.Path, err)
	}
}

func authorize(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

var serviceBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, ioBufferSize)
		return &buf
	},
}

func getServiceIOBuffer() []byte {
	return *serviceBufferPool.Get().(*[]byte)
}

func putServiceIOBuffer(buf []byte) {
	serviceBufferPool.Put(&buf)
}

func joinURL(baseURL, apiPath string) (string, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("receiver URL must include scheme and host")
	}
	u.Path = strings.TrimRight(u.Path, "/") + apiPath
	u.RawQuery = ""
	return u.String(), nil
}
