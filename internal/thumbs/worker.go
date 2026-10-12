package thumbs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/resource"
)

// Worker protocol.
//
// The parent spawns one worker per file and the two exchange two newline
// terminated JSON documents over a pipe pair:
//
//	{"event":"ready",...}      worker is up, has not touched a pixel yet
//	{"event":"result",...}     worker is done
//
// The handshake exists for one reason: the worker must not begin decoding until
// the parent has moved it into a cgroup whose memory.max is actually in force.
// Without the handshake there is a window in which a large decode runs with no
// kernel-enforced ceiling, which is exactly the failure this project exists to
// prevent.
const (
	EventReady  = "ready"
	EventResult = "result"
)

// Exit codes used by the worker subcommand.
const (
	ExitOK      = 0
	ExitSkipped = 4
	ExitFailed  = 5
)

// WorkerEvent is one protocol line.
type WorkerEvent struct {
	Event string `json:"event"`

	// ready
	PID            int    `json:"pid,omitempty"`
	CgroupDir      string `json:"cgroup_dir,omitempty"`
	CgroupMaxBytes int64  `json:"cgroup_max_bytes,omitempty"`
	CgroupKnown    bool   `json:"cgroup_known,omitempty"`

	// result
	Outcome *Outcome `json:"outcome,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// WorkerArgs is the worker's configuration.
type WorkerArgs struct {
	Root          string
	Rel           string
	Out           string
	ExpectSize    int64
	ExpectMtimeNS int64
	Options       Options
	CgroupDir     string
	Handshake     bool
	MaxRuntime    time.Duration
}

// RunWorker executes one thumbnail generation as a standalone process.
//
// It is deliberately a separate process rather than a goroutine: a decoder that
// runs away must be stoppable by the kernel, and the only way to guarantee that
// is for the decoder to be in its own cgroup and its own process group.
func RunWorker(a WorkerArgs, stdin io.Reader, stdout io.Writer) (int, error) {
	enc := json.NewEncoder(stdout)

	// Prove the source path is inside the collection root before opening it.
	srcPath, err := fsutil.SafeRelJoin(a.Root, a.Rel)
	if err != nil {
		_ = enc.Encode(WorkerEvent{Event: EventResult, Error: err.Error(),
			Outcome: &Outcome{State: StateFailed, Reason: "path_outside_root", Detail: err.Error()}})
		return ExitFailed, nil
	}
	srcPath, err = fsutil.ResolveWithin(a.Root, srcPath)
	if err != nil {
		_ = enc.Encode(WorkerEvent{Event: EventResult, Error: err.Error(),
			Outcome: &Outcome{State: StateFailed, Reason: "path_outside_root", Detail: err.Error()}})
		return ExitFailed, nil
	}

	// Report readiness and the cgroup we are actually in.
	ready := WorkerEvent{Event: EventReady, PID: os.Getpid(), CgroupDir: a.CgroupDir}
	if a.CgroupDir != "" {
		if cg, cerr := resource.ReadCgroup(a.CgroupDir); cerr == nil && cg.Present {
			ready.CgroupKnown = true
			if cg.Max > 0 {
				ready.CgroupMaxBytes = cg.Max
			} else {
				ready.CgroupMaxBytes = -1 // "max": no ceiling
			}
		}
	}
	if err := enc.Encode(ready); err != nil {
		return ExitFailed, err
	}

	if a.Handshake {
		buf := make([]byte, 1)
		if _, err := io.ReadFull(stdin, buf); err != nil {
			return ExitFailed, fmt.Errorf("worker: handshake read failed: %w", err)
		}
		if buf[0] != 'g' {
			return ExitSkipped, nil
		}
	}

	if a.MaxRuntime > 0 {
		// Belt and braces: the parent enforces the deadline, but if the parent
		// itself is wedged the worker still stops.
		t := time.AfterFunc(a.MaxRuntime, func() {
			_, _ = fmt.Fprintln(os.Stderr, "worker: internal deadline exceeded, aborting")
			os.Exit(ExitFailed)
		})
		defer t.Stop()
	}

	res := Generate(srcPath, a.Out, a.ExpectSize, a.ExpectMtimeNS, a.Options)
	res.PeakRSSBytes = selfHWM()

	code := ExitOK
	switch res.State {
	case StateOK:
	case StateSkipped:
		code = ExitSkipped
	default:
		code = ExitFailed
	}
	if err := enc.Encode(WorkerEvent{Event: EventResult, Outcome: &res}); err != nil {
		return ExitFailed, err
	}
	return code, nil
}

// selfHWM reads this process's resident high-water mark.
func selfHWM() int64 {
	m, err := resource.ReadProcMemory("/proc", os.Getpid())
	if err != nil {
		return 0
	}
	return m.VMHWMB << 10
}

// ReadEvent reads one protocol line, honouring any deadline set on the
// underlying file by the caller.
func ReadEvent(br *bufio.Reader) (WorkerEvent, error) {
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return WorkerEvent{}, err
	}
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return WorkerEvent{}, errors.New("worker: empty protocol line")
	}
	var ev WorkerEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return WorkerEvent{}, fmt.Errorf("worker: malformed protocol line (%d bytes): %w", len(line), err)
	}
	return ev, nil
}

// ParseFormats turns "jpeg,png,gif,webp" into a Formats value.
func ParseFormats(s string) Formats {
	var f Formats
	for _, p := range strings.Split(s, ",") {
		switch strings.TrimSpace(strings.ToLower(p)) {
		case "jpeg", "jpg":
			f.JPEG = true
		case "png":
			f.PNG = true
		case "gif":
			f.GIF = true
		case "webp":
			f.WebP = true
		}
	}
	if f == (Formats{}) {
		return DefaultFormats()
	}
	return f
}

// FormatNames renders the enabled formats for the worker command line.
func (f Formats) FormatNames() string {
	var out []string
	if f.JPEG {
		out = append(out, "jpeg")
	}
	if f.PNG {
		out = append(out, "png")
	}
	if f.GIF {
		out = append(out, "gif")
	}
	if f.WebP {
		out = append(out, "webp")
	}
	return strings.Join(out, ",")
}

// itoa is a tiny helper keeping flag construction readable.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
