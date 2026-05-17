//go:build !darwin && !linux

package service

import "fmt"

type Daemon struct {
	executable string
	pidPath    string
	logPath    string
}

func NewDaemon(executable, pidPath, logPath string) *Daemon {
	return &Daemon{executable: executable, pidPath: pidPath, logPath: logPath}
}

func (d *Daemon) Start(args []string) error {
	return fmt.Errorf("daemon mode is only implemented on linux and darwin")
}

func (d *Daemon) Stop() error {
	return fmt.Errorf("daemon mode is only implemented on linux and darwin")
}

func (d *Daemon) Status() (bool, int, error) {
	return false, 0, fmt.Errorf("daemon mode is only implemented on linux and darwin")
}
