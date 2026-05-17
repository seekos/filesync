package view

import (
	"embed"
	"fmt"
	"html/template"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

func MustParseTemplates() *template.Template {
	funcs := template.FuncMap{
		"join": strings.Join,
		"percent": func(done, total int) int {
			if total <= 0 {
				return 0
			}
			value := done * 100 / total
			if value < 0 {
				return 0
			}
			if value > 100 {
				return 100
			}
			return value
		},
		"percent64": func(done, total int64) int {
			if total <= 0 {
				return 0
			}
			value := done * 100 / total
			if value < 0 {
				return 0
			}
			if value > 100 {
				return 100
			}
			return int(value)
		},
		"bytes": formatBytes,
		"speed": func(bytesPerSecond int64) string {
			if bytesPerSecond <= 0 {
				return "-"
			}
			return formatBytes(bytesPerSecond) + "/s"
		},
		"eta": func(seconds int64) string {
			if seconds <= 0 {
				return "-"
			}
			d := time.Duration(seconds) * time.Second
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			if d < time.Hour {
				return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
			}
			return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
		},
		"time": func(t *time.Time) string {
			if t == nil {
				return "-"
			}
			return t.Format("2006-01-02 15:04:05")
		},
	}
	return template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
}

func formatBytes(value int64) string {
	if value <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", value, units[unit])
	}
	return fmt.Sprintf("%.1f %s", size, units[unit])
}
