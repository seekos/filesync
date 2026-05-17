package service

import (
	"context"
	"log"
	"sync"
	"time"

	"filesync/internal/model"
	"filesync/internal/repository"
)

type Scheduler struct {
	store   repository.TaskStore
	runner  *SyncRunner
	tick    time.Duration
	pending sync.Map
}

func NewScheduler(store repository.TaskStore, runner *SyncRunner, tick time.Duration) *Scheduler {
	return &Scheduler{store: store, runner: runner, tick: tick}
}

func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		s.runDue(ctx)
		ticker := time.NewTicker(s.tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runDue(ctx)
			}
		}
	}()
}

func (s *Scheduler) runDue(ctx context.Context) {
	now := time.Now()
	for _, task := range s.store.ListTasks() {
		if !task.Enabled || task.Status == model.StatusRunning {
			continue
		}
		if task.LastRunAt != nil && now.Sub(*task.LastRunAt) < task.Interval() {
			continue
		}
		taskID := task.ID
		if _, loaded := s.pending.LoadOrStore(taskID, struct{}{}); loaded {
			continue
		}
		go func() {
			defer s.pending.Delete(taskID)
			if err := s.runner.Run(ctx, taskID); err != nil {
				log.Printf("scheduled sync failed: %v", err)
			}
		}()
	}
}
