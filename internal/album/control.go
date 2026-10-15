package album

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Directory struct {
	Code              string `json:"code"`
	Name              string `json:"name"`
	Root              string `json:"root"`
	Path              string `json:"path"`
	Enabled           bool   `json:"enabled"`
	AllowSharedVolume bool   `json:"allow_shared_volume"`
	Managed           bool   `json:"managed"`
}
type Configuration struct {
	Directories     []Directory `json:"directories"`
	WorkerAvailable bool        `json:"worker_available"`
	Busy            bool        `json:"busy"`
}
type Work struct {
	Operation  string `json:"-"`
	Collection string `json:"collection"`
	Limit      int    `json:"limit"`
	MaxRuntime string `json:"max_runtime"`
	Reason     string `json:"reason"`
}
type WorkResult struct {
	Completed  bool
	StopReason string
	Detail     string
}
type WorkProgress struct {
	PID, PGID        int
	Processed, Total int64
	Detail           string
}

// Control is injected at startup. The web import graph has no subprocess or
// index-writing implementation, only these bounded operator capabilities.
type Control interface {
	Configuration() Configuration
	Configure(context.Context, Directory) error
	RemoveDirectory(context.Context, string) error
	Run(context.Context, Work, func(WorkProgress)) (WorkResult, error)
}

func ValidateWork(w Work) (time.Duration, error) {
	switch w.Operation {
	case "album.scan", "album.thumbs":
		if w.Limit < 1 || w.Limit > 10000 || w.Collection == "" {
			return 0, errors.New("choose a collection and a file limit between 1 and 10000")
		}
		d, err := time.ParseDuration(w.MaxRuntime)
		if err != nil || d < time.Second || d > 2*time.Hour {
			return 0, errors.New("task runtime must be between 1s and 2h")
		}
		return d, nil
	case "album.pause", "album.resume":
		if len(w.Reason) > 200 || strings.ContainsAny(w.Reason, "\r\n\x00") {
			return 0, errors.New("pause reason is invalid")
		}
		return 10 * time.Second, nil
	default:
		return 0, errors.New("unknown album operation")
	}
}
