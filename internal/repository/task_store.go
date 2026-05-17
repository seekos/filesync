package repository

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"filesync/internal/fsmirror"
	"filesync/internal/model"
)

var ErrTaskNotFound = errors.New("task not found")

type TaskStore interface {
	ListTasks() []model.SyncTask
	GetTask(id string) (model.SyncTask, error)
	SaveTask(task model.SyncTask) error
	DeleteTask(id string) error
	UpdateTask(id string, fn func(*model.SyncTask)) error
}

func normalizeTask(task *model.SyncTask, now time.Time) {
	if task.ID == "" {
		task.ID = fmt.Sprintf("%d", now.UnixNano())
	}
	if task.Status == "" {
		task.Status = model.StatusIdle
	}
	if task.IntervalSeconds <= 0 {
		task.IntervalSeconds = 3600
	}
	if task.UploadWorkers <= 0 {
		task.UploadWorkers = 32
	}
	if strings.TrimSpace(task.Type) == "" {
		task.Type = model.TaskTypeReceiver
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = now
	}
}

func validateTask(task *model.SyncTask) error {
	if strings.TrimSpace(task.LocalPath) == "" {
		return fmt.Errorf("local_path is required")
	}
	if task.IsLocal() {
		destRoot, err := fsmirror.LocalMirrorRoot(task.DestinationPath)
		if err != nil {
			return err
		}
		src := filepath.Clean(task.LocalPath)
		if fsmirror.PathsOverlap(src, destRoot) {
			return fmt.Errorf("local_path must not overlap destination_path")
		}
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(task.Type)) {
	case "", model.TaskTypeReceiver:
	default:
		return fmt.Errorf("unknown task type %q", task.Type)
	}
	if strings.TrimSpace(task.ReceiverURL) == "" || strings.TrimSpace(task.DestinationPath) == "" {
		return fmt.Errorf("receiver_url and destination_path are required for receiver tasks")
	}
	return nil
}
