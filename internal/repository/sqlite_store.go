package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"filesync/internal/model"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
	mu sync.Mutex
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &SQLiteStore{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	payload TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_updated_at ON tasks(updated_at);
`)
	return err
}

func (s *SQLiteStore) ListTasks() []model.SyncTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT payload FROM tasks ORDER BY created_at, id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var tasks []model.SyncTask
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return tasks
		}
		var task model.SyncTask
		if err := json.Unmarshal([]byte(payload), &task); err != nil {
			continue
		}
		tasks = append(tasks, task)
	}
	return tasks
}

func (s *SQLiteStore) GetTask(id string) (model.SyncTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getTaskLocked(id)
}

func (s *SQLiteStore) SaveTask(task model.SyncTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if task.ID == "" {
		task.ID = fmt.Sprintf("%d", now.UnixNano())
		task.CreatedAt = now
	}
	task.UpdatedAt = now
	normalizeTask(&task, now)
	if err := validateTask(&task); err != nil {
		return err
	}
	return s.saveTaskLocked(task)
}

func (s *SQLiteStore) DeleteTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrTaskNotFound
	}
	return nil
}

func (s *SQLiteStore) UpdateTask(id string, fn func(*model.SyncTask)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, err := s.getTaskLocked(id)
	if err != nil {
		return err
	}
	fn(&task)
	task.UpdatedAt = time.Now()
	normalizeTask(&task, task.UpdatedAt)
	if err := validateTask(&task); err != nil {
		return err
	}
	return s.saveTaskLocked(task)
}

func (s *SQLiteStore) getTaskLocked(id string) (model.SyncTask, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload FROM tasks WHERE id = ?`, id).Scan(&payload)
	if err == sql.ErrNoRows {
		return model.SyncTask{}, ErrTaskNotFound
	}
	if err != nil {
		return model.SyncTask{}, err
	}
	var task model.SyncTask
	if err := json.Unmarshal([]byte(payload), &task); err != nil {
		return model.SyncTask{}, err
	}
	return task, nil
}

func (s *SQLiteStore) saveTaskLocked(task model.SyncTask) error {
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO tasks (id, payload, created_at, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`,
		task.ID,
		string(payload),
		task.CreatedAt.Format(time.RFC3339Nano),
		task.UpdatedAt.Format(time.RFC3339Nano),
	)
	return err
}
