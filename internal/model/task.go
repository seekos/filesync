package model

import (
	"strings"
	"time"
)

const (
	TaskTypeReceiver = "receiver"
	TaskTypeLocal    = "local"
)

type TaskStatus string

const (
	StatusIdle    TaskStatus = "idle"
	StatusRunning TaskStatus = "running"
	StatusSuccess TaskStatus = "success"
	StatusFailed  TaskStatus = "failed"
	StatusStopped TaskStatus = "stopped"
)

type SyncTask struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Type             string     `json:"type"`
	LocalPath        string     `json:"local_path"`
	ReceiverURL      string     `json:"receiver_url"`
	DestinationPath  string     `json:"destination_path"`
	AuthToken        string     `json:"auth_token"`
	Excludes         []string   `json:"excludes"`
	DeleteExtraneous bool       `json:"delete_extraneous"`
	Enabled          bool       `json:"enabled"`
	IntervalSeconds  int        `json:"interval_seconds"`
	UploadWorkers    int        `json:"upload_workers"`
	LastRunAt        *time.Time `json:"last_run_at,omitempty"`
	LastSuccessAt    *time.Time `json:"last_success_at,omitempty"`
	LastError        string     `json:"last_error"`
	Status           TaskStatus `json:"status"`
	CurrentStep      string     `json:"current_step"`
	CurrentFile      string     `json:"current_file"`
	ScannedFiles     int        `json:"scanned_files"`
	NeededFiles      int        `json:"needed_files"`
	UploadedFiles    int        `json:"uploaded_files"`
	NeededBytes      int64      `json:"needed_bytes"`
	UploadedBytes    int64      `json:"uploaded_bytes"`
	UploadSpeedBps   int64      `json:"upload_speed_bps"`
	ETASeconds       int64      `json:"eta_seconds"`
	CurrentFileSize  int64      `json:"current_file_size"`
	CurrentFileBytes int64      `json:"current_file_bytes"`
	LastProgressAt   *time.Time `json:"last_progress_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type AppConfig struct {
	Tasks []SyncTask `json:"tasks"`
}

func (t SyncTask) IsLocal() bool {
	return strings.EqualFold(strings.TrimSpace(t.Type), TaskTypeLocal)
}

func (t SyncTask) RemoteAddr() string {
	if t.IsLocal() {
		if t.DestinationPath != "" {
			return "本地 -> " + t.DestinationPath
		}
		return "本地"
	}
	if t.DestinationPath == "" {
		return t.ReceiverURL
	}
	return t.ReceiverURL + " -> " + t.DestinationPath
}

func (t SyncTask) Interval() time.Duration {
	if t.IntervalSeconds <= 0 {
		return time.Hour
	}
	return time.Duration(t.IntervalSeconds) * time.Second
}
