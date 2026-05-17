//go:build darwin || linux

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

type Daemon struct {
	executable string
	pidPath    string
	logPath    string
}

func NewDaemon(executable, pidPath, logPath string) *Daemon {
	return &Daemon{executable: executable, pidPath: pidPath, logPath: logPath}
}

func (d *Daemon) Start(args []string) error {
	running, pid, err := d.Status()
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("filesync already running, pid=%d", pid)
	}
	if err := os.MkdirAll(filepath.Dir(d.pidPath), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.logPath), 0755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(d.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	proc, err := os.StartProcess(d.executable, append([]string{d.executable}, args...), &os.ProcAttr{
		Files: []*os.File{devNull, logFile, logFile},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(d.pidPath, []byte(strconv.Itoa(proc.Pid)), 0600); err != nil {
		return err
	}
	fmt.Printf("filesync started, pid=%d\n", proc.Pid)
	return nil
}

func (d *Daemon) Stop() error {
	running, pid, err := d.Status()
	if err != nil {
		return err
	}
	if !running {
		_ = os.Remove(d.pidPath)
		fmt.Println("filesync is already stopped")
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	_ = os.Remove(d.pidPath)
	fmt.Printf("filesync stopped, pid=%d\n", pid)
	return nil
}

func (d *Daemon) Status() (bool, int, error) {
	data, err := os.ReadFile(d.pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	pid, err := strconv.Atoi(string(bytesTrimSpace(data)))
	if err != nil {
		return false, 0, err
	}
	if pid <= 0 {
		return false, pid, nil
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, pid, nil
		}
		return false, pid, err
	}
	return true, pid, nil
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 {
		last := b[len(b)-1]
		if last != ' ' && last != '\n' && last != '\t' && last != '\r' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}
