package repository

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filesync/internal/model"
)

func DefaultRuntimeConfig(stateDir string) model.RuntimeConfig {
	return model.RuntimeConfig{
		Sender: model.SenderConfig{
			Listen:           "127.0.0.1:1985",
			DBPath:           "filesync.db",
			SchedulerSeconds: 30,
		},
		Receiver: model.ReceiverConfig{
			Listen:      ":1987",
			StorageRoot: filepath.Join(stateDir, "storage"),
		},
		Daemon: model.DaemonConfig{
			PIDPath: filepath.Join(stateDir, "filesync.pid"),
			LogPath: filepath.Join(stateDir, "filesync.log"),
		},
	}
}

func LoadRuntimeConfig(path, stateDir string) (model.RuntimeConfig, error) {
	cfg := DefaultRuntimeConfig(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := ensureReceiverToken(&cfg); err != nil {
			return cfg, err
		}
		return cfg, SaveRuntimeConfig(path, cfg)
	}
	if err != nil {
		return cfg, err
	}
	if strings.TrimSpace(string(data)) == "" {
		if err := ensureReceiverToken(&cfg); err != nil {
			return cfg, err
		}
		return cfg, SaveRuntimeConfig(path, cfg)
	}
	if err := parseRuntimeConfig(string(data), &cfg); err != nil {
		return cfg, err
	}
	normalizeRuntimeConfig(&cfg, stateDir)
	if strings.TrimSpace(cfg.Receiver.Token) == "" {
		if err := ensureReceiverToken(&cfg); err != nil {
			return cfg, err
		}
		if err := SaveRuntimeConfig(path, cfg); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func SaveRuntimeConfig(path string, cfg model.RuntimeConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data := []byte(formatRuntimeConfig(cfg))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseRuntimeConfig(data string, cfg *model.RuntimeConfig) error {
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(data))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(stripInlineComment(scanner.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("line %d: expected key = value", lineNo)
		}
		if err := assignRuntimeConfigValue(cfg, section, strings.TrimSpace(key), strings.TrimSpace(raw)); err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	return scanner.Err()
}

func assignRuntimeConfigValue(cfg *model.RuntimeConfig, section, key, raw string) error {
	switch section {
	case "sender":
		switch key {
		case "listen":
			return parseString(raw, &cfg.Sender.Listen)
		case "web_token":
			return parseString(raw, &cfg.Sender.WebToken)
		case "db_path":
			return parseString(raw, &cfg.Sender.DBPath)
		case "scheduler_seconds":
			return parseInt(raw, &cfg.Sender.SchedulerSeconds)
		}
	case "receiver":
		switch key {
		case "listen":
			return parseString(raw, &cfg.Receiver.Listen)
		case "storage_root":
			return parseString(raw, &cfg.Receiver.StorageRoot)
		case "token":
			return parseString(raw, &cfg.Receiver.Token)
		case "allowed_dirs":
			return json.Unmarshal([]byte(raw), &cfg.Receiver.AllowedDirs)
		}
	case "daemon":
		switch key {
		case "pid_path":
			return parseString(raw, &cfg.Daemon.PIDPath)
		case "log_path":
			return parseString(raw, &cfg.Daemon.LogPath)
		}
	}
	return fmt.Errorf("unknown config key %q in section %q", key, section)
}

func normalizeRuntimeConfig(cfg *model.RuntimeConfig, stateDir string) {
	defaults := DefaultRuntimeConfig(stateDir)
	if strings.TrimSpace(cfg.Sender.Listen) == "" {
		cfg.Sender.Listen = defaults.Sender.Listen
	}
	if strings.TrimSpace(cfg.Sender.DBPath) == "" {
		cfg.Sender.DBPath = defaults.Sender.DBPath
	}
	cfg.Sender.DBPath = slashPath(cfg.Sender.DBPath)
	if cfg.Sender.SchedulerSeconds <= 0 {
		cfg.Sender.SchedulerSeconds = defaults.Sender.SchedulerSeconds
	}
	if strings.TrimSpace(cfg.Receiver.Listen) == "" {
		cfg.Receiver.Listen = defaults.Receiver.Listen
	}
	if strings.TrimSpace(cfg.Receiver.StorageRoot) == "" {
		cfg.Receiver.StorageRoot = defaults.Receiver.StorageRoot
	}
	cfg.Receiver.StorageRoot = slashPath(cfg.Receiver.StorageRoot)
	for i := range cfg.Receiver.AllowedDirs {
		cfg.Receiver.AllowedDirs[i] = slashPath(cfg.Receiver.AllowedDirs[i])
	}
	if strings.TrimSpace(cfg.Daemon.PIDPath) == "" {
		cfg.Daemon.PIDPath = defaults.Daemon.PIDPath
	}
	cfg.Daemon.PIDPath = slashPath(cfg.Daemon.PIDPath)
	if strings.TrimSpace(cfg.Daemon.LogPath) == "" {
		cfg.Daemon.LogPath = defaults.Daemon.LogPath
	}
	cfg.Daemon.LogPath = slashPath(cfg.Daemon.LogPath)
}

func formatRuntimeConfig(cfg model.RuntimeConfig) string {
	var b strings.Builder
	b.WriteString("# FileSync runtime configuration\n")
	b.WriteString("[sender]\n")
	writeString(&b, "listen", cfg.Sender.Listen)
	writeString(&b, "web_token", cfg.Sender.WebToken)
	writeConfigPath(&b, "db_path", cfg.Sender.DBPath)
	writeInt(&b, "scheduler_seconds", cfg.Sender.SchedulerSeconds)
	b.WriteByte('\n')
	b.WriteString("[receiver]\n")
	writeString(&b, "listen", cfg.Receiver.Listen)
	writeConfigPath(&b, "storage_root", cfg.Receiver.StorageRoot)
	writeString(&b, "token", cfg.Receiver.Token)
	writeConfigPathArray(&b, "allowed_dirs", cfg.Receiver.AllowedDirs)
	b.WriteByte('\n')
	b.WriteString("[daemon]\n")
	writeConfigPath(&b, "pid_path", cfg.Daemon.PIDPath)
	writeConfigPath(&b, "log_path", cfg.Daemon.LogPath)
	return b.String()
}

func writeConfigPath(b *strings.Builder, key, value string) {
	writeString(b, key, slashPath(value))
}

func writeConfigPathArray(b *strings.Builder, key string, values []string) {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = slashPath(value)
	}
	writeStringArray(b, key, out)
}

func slashPath(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(filepath.ToSlash(value)), "\\", "/")
}

func ensureReceiverToken(cfg *model.RuntimeConfig) error {
	if strings.TrimSpace(cfg.Receiver.Token) != "" {
		return nil
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	cfg.Receiver.Token = token
	return nil
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
