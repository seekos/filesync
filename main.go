package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filesync/internal/controller"
	"filesync/internal/model"
	"filesync/internal/repository"
	"filesync/internal/service"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("filesync: %v", err)
	}
}

func run() error {
	defaultConfig := defaultConfigPath()

	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	configPath := defaultConfig
	daemonize := false
	fs.StringVar(&configPath, "config", configPath, "config file path")
	fs.StringVar(&configPath, "c", configPath, "config file path")
	fs.BoolVar(&daemonize, "d", daemonize, "run as daemon")
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "local", "mirror":
		return runLocalMirror(args)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	cfg, err := repository.LoadRuntimeConfig(configPath, defaultStateDir())
	if err != nil {
		return err
	}
	daemon := service.NewDaemon(os.Args[0], cfg.Daemon.PIDPath, cfg.Daemon.LogPath)
	if daemonize {
		return daemon.Start(daemonArgs(cmd, configPath))
	}
	switch cmd {
	case "serve":
		return serveBoth(cfg)
	case "sender":
		return serveSender(cfg)
	case "receiver":
		return serveReceiver(cfg)
	case "stop":
		return daemon.Stop()
	case "status":
		running, pid, err := daemon.Status()
		if err != nil {
			return err
		}
		if running {
			fmt.Printf("filesync is running, pid=%d\n", pid)
		} else {
			fmt.Println("filesync is stopped")
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q, use sender/receiver/local/stop/status", cmd)
	}
}

func runLocalMirror(args []string) error {
	fs := flag.NewFlagSet("local", flag.ExitOnError)
	from := fs.String("from", "", "source directory")
	to := fs.String("to", "", "destination mirror root (must be absolute)")
	deleteExtra := fs.Bool("delete-extraneous", false, "delete files under destination that are not in the source tree")
	workers := fs.Int("workers", 32, "concurrent file copies (1-128)")
	excludes := fs.String("excludes", "", "comma-separated exclude patterns, e.g. \"*.tmp,.git/\"")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return fmt.Errorf("local mirror requires -from and -to")
	}
	w := *workers
	if w < 1 {
		w = 1
	}
	if w > 128 {
		w = 128
	}
	tmpDB := filepath.Join(os.TempDir(), fmt.Sprintf("filesync-local-%d.db", time.Now().UnixNano()))
	defer func() { _ = os.Remove(tmpDB) }()
	store, err := repository.NewSQLiteStore(tmpDB)
	if err != nil {
		return err
	}
	defer store.Close()
	task := model.SyncTask{
		Name:             "local-mirror",
		Type:             model.TaskTypeLocal,
		LocalPath:        *from,
		DestinationPath:  *to,
		DeleteExtraneous: *deleteExtra,
		Enabled:          false,
		IntervalSeconds:  3600,
		UploadWorkers:    w,
		Excludes:         splitCommaPatterns(*excludes),
		Status:           model.StatusIdle,
	}
	if err := store.SaveTask(task); err != nil {
		return err
	}
	tasks := store.ListTasks()
	if len(tasks) != 1 {
		return fmt.Errorf("internal error: expected one task after save")
	}
	runner := service.NewSyncRunner(store)
	return runner.Run(context.Background(), tasks[0].ID)
}

func splitCommaPatterns(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func serveSender(cfg model.RuntimeConfig) error {
	endpoint, closeStore, err := senderEndpoint(cfg)
	if err != nil {
		return err
	}
	defer closeStore()
	return serveHTTPServices(endpoint)
}

func serveReceiver(cfg model.RuntimeConfig) error {
	endpoint, err := receiverEndpoint(cfg)
	if err != nil {
		return err
	}
	return serveHTTPServices(endpoint)
}

func serveBoth(cfg model.RuntimeConfig) error {
	sender, closeStore, err := senderEndpoint(cfg)
	if err != nil {
		return err
	}
	defer closeStore()
	receiver, err := receiverEndpoint(cfg)
	if err != nil {
		return err
	}
	return serveHTTPServices(sender, receiver)
}

type httpEndpoint struct {
	name      string
	listen    string
	handler   http.Handler
	scheduler *service.Scheduler
}

func senderEndpoint(cfg model.RuntimeConfig) (httpEndpoint, func(), error) {
	if isPublicListen(cfg.Sender.Listen) && strings.TrimSpace(cfg.Sender.WebToken) == "" {
		return httpEndpoint{}, nil, fmt.Errorf("public sender listen requires sender.web_token in config.toml")
	}
	store, err := repository.NewSQLiteStore(cfg.Sender.DBPath)
	if err != nil {
		return httpEndpoint{}, nil, err
	}

	runner := service.NewSyncRunner(store)
	tick := time.Duration(cfg.Sender.SchedulerSeconds) * time.Second
	scheduler := service.NewScheduler(store, runner, tick)
	web := controller.NewWebController(store, runner, cfg.Sender.WebToken)

	mux := http.NewServeMux()
	web.Register(mux)

	return httpEndpoint{
		name:      "filesync sender web console",
		listen:    cfg.Sender.Listen,
		handler:   mux,
		scheduler: scheduler,
	}, func() { _ = store.Close() }, nil
}

func receiverEndpoint(cfg model.RuntimeConfig) (httpEndpoint, error) {
	if strings.TrimSpace(cfg.Receiver.Token) == "" {
		return httpEndpoint{}, fmt.Errorf("receiver requires receiver.token in config.toml")
	}
	if err := os.MkdirAll(cfg.Receiver.StorageRoot, 0755); err != nil {
		return httpEndpoint{}, err
	}
	mux := http.NewServeMux()
	controller.NewReceiverController(cfg.Receiver.StorageRoot, cfg.Receiver.Token, cfg.Receiver.AllowedDirs).Register(mux)
	return httpEndpoint{
		name:    "filesync receiver",
		listen:  cfg.Receiver.Listen,
		handler: mux,
	}, nil
}

func serveHTTPServices(endpoints ...httpEndpoint) error {
	if len(endpoints) == 0 {
		return fmt.Errorf("no HTTP services configured")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	servers := make([]*http.Server, 0, len(endpoints))
	errCh := make(chan error, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.scheduler != nil {
			endpoint.scheduler.Start(ctx)
		}
		server := newHTTPServer(endpoint.listen, endpoint.handler)
		servers = append(servers, server)
		go func(name string, srv *http.Server) {
			log.Printf("%s listening on %s", name, srv.Addr)
			errCh <- srv.ListenAndServe()
		}(endpoint.name, server)
	}

	for {
		select {
		case <-ctx.Done():
			return shutdownServers(servers)
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				continue
			}
			stop()
			_ = shutdownServers(servers)
			return err
		}
	}
}

func newHTTPServer(listen string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func shutdownServers(servers []*http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var result error
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil && result == nil {
			result = err
		}
	}
	return result
}

func daemonArgs(cmd, configPath string) []string {
	switch cmd {
	case "sender", "receiver":
		return []string{cmd, "-c", configPath}
	default:
		return []string{"-c", configPath}
	}
}

func isPublicListen(listen string) bool {
	host := listen
	if strings.HasPrefix(listen, ":") {
		return true
	}
	if h, _, ok := strings.Cut(listen, ":"); ok {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "" || host == "0.0.0.0" || host == "::"
}

func defaultStateDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".filesync")
	}
	return ".filesync"
}

func defaultConfigPath() string {
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "config.toml")
	}
	return "config.toml"
}
