package controller

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"filesync/internal/model"
	"filesync/internal/repository"
	"filesync/internal/service"
	"filesync/internal/view"
)

type WebController struct {
	store     repository.TaskStore
	runner    *service.SyncRunner
	templates *template.Template
	webToken  string
}

type taskRow struct {
	model.SyncTask
	ReceiverStatus string
}

func NewWebController(store repository.TaskStore, runner *service.SyncRunner, webToken ...string) *WebController {
	token := ""
	if len(webToken) > 0 {
		token = strings.TrimSpace(webToken[0])
	}
	return &WebController{
		store:     store,
		runner:    runner,
		templates: view.MustParseTemplates(),
		webToken:  token,
	}
}

func (c *WebController) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", c.requireAuth(c.dashboard))
	mux.HandleFunc("/tasks/new", c.requireAuth(c.newTask))
	mux.HandleFunc("/tasks/create", c.requireAuth(c.createTask))
	mux.HandleFunc("/tasks/", c.requireAuth(c.taskAction))
}

func (c *WebController) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.webToken == "" {
			next(w, r)
			return
		}
		_, password, ok := r.BasicAuth()
		if ok && password == c.webToken {
			next(w, r)
			return
		}
		if r.Header.Get("Authorization") == "Bearer "+c.webToken {
			next(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="FileSync"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (c *WebController) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tasks := c.store.ListTasks()
	hasRunning := false
	for _, task := range tasks {
		if task.Status == model.StatusRunning {
			hasRunning = true
			break
		}
	}
	rows := c.buildTaskRows(r.Context(), tasks)
	c.render(w, "dashboard.html", map[string]any{"Tasks": rows, "HasRunning": hasRunning})
}

func (c *WebController) buildTaskRows(ctx context.Context, tasks []model.SyncTask) []taskRow {
	rows := make([]taskRow, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		rows[i] = taskRow{SyncTask: task, ReceiverStatus: "未配置"}
		if task.IsLocal() {
			rows[i].ReceiverStatus = "本地"
			continue
		}
		if strings.TrimSpace(task.ReceiverURL) == "" {
			continue
		}
		wg.Add(1)
		go func(i int, task model.SyncTask) {
			defer wg.Done()
			if receiverOnline(ctx, task.ReceiverURL) {
				rows[i].ReceiverStatus = "在线"
				return
			}
			rows[i].ReceiverStatus = "离线"
		}(i, task)
	}
	wg.Wait()
	return rows
}

func receiverOnline(parent context.Context, receiverURL string) bool {
	endpoint, err := receiverHealthURL(receiverURL)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 800*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func receiverHealthURL(receiverURL string) (string, error) {
	u, err := url.Parse(strings.TrimRight(receiverURL, "/"))
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("receiver URL must include scheme and host")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/health"
	u.RawQuery = ""
	return u.String(), nil
}

func (c *WebController) newTask(w http.ResponseWriter, r *http.Request) {
	c.render(w, "form.html", map[string]any{
		"Title": "新增同步目录",
		"Task":  model.SyncTask{Enabled: true, IntervalSeconds: 3600, UploadWorkers: 32, Type: model.TaskTypeReceiver},
		"Mode":  "create",
	})
}

func (c *WebController) createTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	task := parseTaskForm(r)
	if err := c.store.SaveTask(task); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (c *WebController) taskAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/tasks/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]
	switch action {
	case "edit":
		task, err := c.store.GetTask(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		c.render(w, "form.html", map[string]any{"Title": "编辑同步目录", "Task": task, "Mode": "edit"})
	case "update":
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		task := parseTaskForm(r)
		old, err := c.store.GetTask(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		task.ID = old.ID
		task.CreatedAt = old.CreatedAt
		task.LastRunAt = old.LastRunAt
		task.LastSuccessAt = old.LastSuccessAt
		task.LastError = old.LastError
		task.Status = old.Status
		if err := c.store.SaveTask(task); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "delete":
		if r.Method == http.MethodPost {
			c.runner.Stop(id)
			_ = c.store.DeleteTask(id)
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "toggle":
		if r.Method == http.MethodPost {
			_ = c.store.UpdateTask(id, func(t *model.SyncTask) {
				t.Enabled = !t.Enabled
				if !t.Enabled {
					c.runner.Stop(id)
					if t.Status == model.StatusRunning {
						t.Status = model.StatusStopped
						t.CurrentStep = "已停止"
						t.CurrentFile = ""
						t.UploadSpeedBps = 0
						t.ETASeconds = 0
					}
				}
			})
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "run":
		if r.Method == http.MethodPost {
			go c.runner.Run(context.Background(), id)
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "stop":
		if r.Method == http.MethodPost {
			if !c.runner.Stop(id) {
				_ = c.store.UpdateTask(id, func(t *model.SyncTask) {
					if t.Status == model.StatusRunning {
						t.Status = model.StatusStopped
						t.CurrentStep = "已停止"
						t.UploadSpeedBps = 0
						t.ETASeconds = 0
					}
				})
			}
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func (c *WebController) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func parseTaskForm(r *http.Request) model.SyncTask {
	_ = r.ParseForm()
	interval, _ := strconv.Atoi(r.FormValue("interval_seconds"))
	workers, _ := strconv.Atoi(r.FormValue("upload_workers"))
	return model.SyncTask{
		Name:             strings.TrimSpace(r.FormValue("name")),
		Type:             strings.TrimSpace(r.FormValue("task_type")),
		LocalPath:        slashPathInput(r.FormValue("local_path")),
		ReceiverURL:      strings.TrimRight(strings.TrimSpace(r.FormValue("receiver_url")), "/"),
		DestinationPath:  slashPathInput(r.FormValue("destination_path")),
		AuthToken:        strings.TrimSpace(r.FormValue("auth_token")),
		Excludes:         splitLines(r.FormValue("excludes")),
		DeleteExtraneous: r.FormValue("delete_extraneous") == "on",
		Enabled:          r.FormValue("enabled") == "on",
		IntervalSeconds:  interval,
		UploadWorkers:    workers,
		Status:           model.StatusIdle,
	}
}

func slashPathInput(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
}

func splitLines(value string) []string {
	var out []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
