package controller

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"filesync/internal/repository"
	"filesync/internal/service"
)

func TestDashboardRenders(t *testing.T) {
	mux := testMux(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "文件同步任务") {
		t.Fatalf("dashboard did not render expected title")
	}
}

func TestCreateTask(t *testing.T) {
	mux := testMux(t)

	form := strings.NewReader("name=demo&local_path=/tmp&receiver_url=http://127.0.0.1:1987&destination_path=server-a/app&interval_seconds=3600&enabled=on")
	req := httptest.NewRequest(http.MethodPost, "/tasks/create", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
}

func testMux(t *testing.T) *http.ServeMux {
	t.Helper()
	store, err := repository.NewSQLiteStore(filepath.Join(t.TempDir(), "filesync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctrl := NewWebController(store, service.NewSyncRunner(store))
	mux := http.NewServeMux()
	ctrl.Register(mux)
	return mux
}
