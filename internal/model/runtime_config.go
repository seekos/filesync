package model

type RuntimeConfig struct {
	Sender   SenderConfig
	Receiver ReceiverConfig
	Daemon   DaemonConfig
}

type SenderConfig struct {
	Listen           string
	WebToken         string
	DBPath           string
	SchedulerSeconds int
}

type ReceiverConfig struct {
	Listen      string
	StorageRoot string
	Token       string
	AllowedDirs []string
}

type DaemonConfig struct {
	PIDPath string
	LogPath string
}
