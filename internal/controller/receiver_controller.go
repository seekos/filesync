package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"filesync/internal/fsmirror"
	"filesync/internal/model"
)

const maxReceiverBodyBytes = 256 * 1024 * 1024

type ReceiverController struct {
	storageRoot string
	token       string
	allowedDirs []string
}

func NewReceiverController(storageRoot, token string, allowedDirs ...[]string) *ReceiverController {
	dirs := []string(nil)
	if len(allowedDirs) > 0 {
		dirs = append(dirs, allowedDirs[0]...)
	}
	return &ReceiverController{
		storageRoot: filepath.Clean(storageRoot),
		token:       token,
		allowedDirs: dirs,
	}
}

func (c *ReceiverController) Register(mux *http.ServeMux) {
	mux.HandleFunc("/health", c.health)
	mux.HandleFunc("/api/v1/manifest", c.manifest)
	mux.HandleFunc("/api/v1/files", c.upload)
	mux.HandleFunc("/api/v1/begin", c.beginUpload)
	mux.HandleFunc("/api/v1/chunks", c.uploadChunk)
	mux.HandleFunc("/api/v1/complete", c.completeUpload)
	mux.HandleFunc("/api/v1/abort", c.abortUpload)
	mux.HandleFunc("/api/v1/prune", c.prune)
}

func (c *ReceiverController) health(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (c *ReceiverController) manifest(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req model.ManifestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base, err := fsmirror.ReceiverDestinationBase(c.storageRoot, req.Destination, c.allowedDirs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	needed, err := fsmirror.NeededRelativePaths(base, req.Files)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, model.ManifestResponse{Needed: needed})
}

func (c *ReceiverController) upload(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !limitRequestBody(w, r, maxReceiverBodyBytes) {
		return
	}
	destination := r.URL.Query().Get("destination")
	relPath := r.URL.Query().Get("path")
	base, err := fsmirror.ReceiverDestinationBase(c.storageRoot, destination, c.allowedDirs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := fsmirror.SafeJoin(base, relPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	modeValue, _ := strconv.ParseUint(r.URL.Query().Get("mode"), 10, 32)
	if modeValue == 0 {
		modeValue = 0644
	}
	modValue, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	modTime := time.Now()
	if modValue > 0 {
		modTime = time.Unix(0, modValue)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp := target + ".uploading"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(modeValue)&0777)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buf := getIOBuffer()
	_, copyErr := io.CopyBuffer(out, r.Body, buf)
	putIOBuffer(buf)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		http.Error(w, copyErr.Error(), http.StatusInternalServerError)
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		http.Error(w, closeErr.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Chmod(tmp, os.FileMode(modeValue)&0777)
	_ = os.Chtimes(tmp, modTime, modTime)
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, model.UploadResponse{Path: relPath, OK: true})
}

func (c *ReceiverController) beginUpload(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req model.BeginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base, err := fsmirror.ReceiverDestinationBase(c.storageRoot, req.Destination, c.allowedDirs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := fsmirror.SafeJoin(base, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Size < 0 {
		http.Error(w, "invalid size", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	mode := os.FileMode(req.Mode) & 0777
	if mode == 0 {
		mode = 0644
	}
	tmp := target + ".uploading"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := out.Truncate(req.Size); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, model.UploadResponse{Path: req.Path, OK: true})
}

func (c *ReceiverController) uploadChunk(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !limitRequestBody(w, r, maxReceiverBodyBytes) {
		return
	}
	base, err := fsmirror.ReceiverDestinationBase(c.storageRoot, r.URL.Query().Get("destination"), c.allowedDirs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := fsmirror.SafeJoin(base, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "invalid offset", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp := target + ".uploading"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writer := &offsetFileWriter{file: out, offset: offset}
	buf := getIOBuffer()
	_, copyErr := io.CopyBuffer(writer, r.Body, buf)
	putIOBuffer(buf)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		http.Error(w, copyErr.Error(), http.StatusInternalServerError)
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		http.Error(w, closeErr.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, model.UploadResponse{Path: r.URL.Query().Get("path"), OK: true})
}

func (c *ReceiverController) abortUpload(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req model.AbortRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base, err := fsmirror.ReceiverDestinationBase(c.storageRoot, req.Destination, c.allowedDirs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := fsmirror.SafeJoin(base, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.Remove(target + ".uploading"); err != nil && !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, model.UploadResponse{Path: req.Path, OK: true})
}

func limitRequestBody(w http.ResponseWriter, r *http.Request, maxBytes int64) bool {
	if r.ContentLength < 0 || r.ContentLength > maxBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return true
}

func (c *ReceiverController) completeUpload(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req model.CompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base, err := fsmirror.ReceiverDestinationBase(c.storageRoot, req.Destination, c.allowedDirs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := fsmirror.SafeJoin(base, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tmp := target + ".uploading"
	file, err := os.OpenFile(tmp, os.O_RDWR, 0644)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := file.Truncate(req.Size); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	mode := os.FileMode(req.Mode) & 0777
	if mode == 0 {
		mode = 0644
	}
	modTime := time.Now()
	if req.ModTime > 0 {
		modTime = time.Unix(0, req.ModTime)
	}
	_ = os.Chmod(tmp, mode)
	_ = os.Chtimes(tmp, modTime, modTime)
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, model.UploadResponse{Path: req.Path, OK: true})
}

func (c *ReceiverController) prune(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req model.PruneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	root, err := fsmirror.ReceiverDestinationBase(c.storageRoot, req.Destination, c.allowedDirs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := fsmirror.PruneKeepingRelative(root, req.Files); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (c *ReceiverController) authorized(w http.ResponseWriter, r *http.Request) bool {
	if c.token == "" {
		http.Error(w, "receiver token is required", http.StatusUnauthorized)
		return false
	}
	if r.Header.Get("Authorization") == "Bearer "+c.token {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

type offsetFileWriter struct {
	file   *os.File
	offset int64
}

func (w *offsetFileWriter) Write(p []byte) (int, error) {
	n, err := w.file.WriteAt(p, w.offset)
	w.offset += int64(n)
	return n, err
}

var receiverBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 1024*1024)
		return &buf
	},
}

func getIOBuffer() []byte {
	return *receiverBufferPool.Get().(*[]byte)
}

func putIOBuffer(buf []byte) {
	receiverBufferPool.Put(&buf)
}
