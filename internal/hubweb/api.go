package hubweb

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/resource"
	"github.com/YoisakiKnd/nasctl/internal/suzu"
)

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------

// overviewResponse is the console's front page: what is wrong, what is running,
// what changed, and what the machine looks like.
type overviewResponse struct {
	Version string `json:"version"`
	Now     int64  `json:"now"`
	// Mode is the effective mode and everything contributing to it.
	Mode domain.ModeState `json:"mode"`
	// AllModes lists every stored mode, including expired ones, so the page can
	// show a lapsed window rather than pretending it never existed.
	AllModes []domain.ModeState `json:"all_modes"`
	// Warnings are the things an operator must see first. An empty list is
	// reported as an empty list, not omitted, so the UI cannot render absence as
	// health by accident.
	Warnings []string `json:"warnings"`
	// Attention lists jobs whose state is unknown, interrupted or failed.
	Attention []jobSummary `json:"attention"`
	// Running lists jobs that hold a slot.
	Running []jobSummary `json:"running"`
	// Counts is the job table by state.
	Counts map[string]int `json:"job_counts"`

	Files rootsResponse `json:"files"`

	// Resources is the machine's own view. It is included because the console's
	// first job is to be honest about whether the machine can do work at all.
	Resources *resourceView `json:"resources,omitempty"`
	// ResourceError explains why the resource view is absent, rather than
	// leaving the UI to guess.
	ResourceError string `json:"resource_error,omitempty"`

	// ReadOnly reports whether this console refuses work that changes state.
	ReadOnly bool `json:"read_only"`
	// AllowAIHeavy reports whether agents may start heavy work. It is surfaced
	// because an operator should be able to see this switch without reading the
	// config file.
	AllowAIHeavy bool `json:"allow_ai_heavy"`
	// Locks lists held mutex classes.
	Locks map[string]string `json:"locks,omitempty"`
	// DataAsOf is when this snapshot was taken, so the page can say how old it
	// is instead of implying it is live.
	DataAsOf time.Time `json:"data_as_of"`
}

// resourceView is the machine's condition, as the console sees it.
type resourceView struct {
	MemAvailableKB int64 `json:"mem_available_kb"`
	MemTotalKB     int64 `json:"mem_total_kb"`
	SwapUsedKB     int64 `json:"swap_used_kb"`
	SwapTotalKB    int64 `json:"swap_total_kb"`
	// WorkerRSSBytes is the supervised worker's resident size, when one exists.
	WorkerRSSBytes int64 `json:"worker_rss_bytes,omitempty"`
	// Verdict is what the guard would decide right now.
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// PSIMemorySomeAvg10 and PSIIOSomeAvg10 are pressure-stall percentages.
	PSIMemorySomeAvg10 float64  `json:"psi_memory_some_avg10,omitempty"`
	PSIIOSomeAvg10     float64  `json:"psi_io_some_avg10,omitempty"`
	Notes              []string `json:"notes,omitempty"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := overviewResponse{
		Version:   s.Version,
		Now:       time.Now().Unix(),
		DataAsOf:  time.Now(),
		Warnings:  []string{},
		Attention: []jobSummary{},
		Running:   []jobSummary{},
		Counts:    map[string]int{},
		AllModes:  []domain.ModeState{},
	}

	set, err := s.Jobs.Modes(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	now := time.Now()
	out.Mode = set.Effective(now)
	// An empty mode set is reported as an empty list, not null: the UI must be
	// able to tell "no modes are set" from "the modes were not reported".
	if set.States == nil {
		out.AllModes = []domain.ModeState{}
	} else {
		out.AllModes = set.States
	}

	if !out.Mode.AllowsHeavyWork() {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"heavy work is stopped: mode %q is in force (%s)%s",
			out.Mode.Mode, out.Mode.Source, reasonSuffix(out.Mode.Reason)))
	}

	p := principalOf(r.Context())
	owner := ""
	if p.Kind == domain.PrincipalUser && !p.DeviceAdmin() {
		owner = p.String()
	}
	counts, err := s.Jobs.Store().CountByOwner(ctx, owner)
	if err == nil {
		out.Counts = counts
	}
	jobs, err := s.Jobs.Store().Jobs(ctx, hubstore.JobFilter{Open: true, Limit: 200, Principal: owner})
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	for _, j := range jobs {
		js := toJobSummary(j)
		if js.Attention {
			out.Attention = append(out.Attention, js)
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"job %s (%s) is %s", j.ID, j.Op, j.State))
		} else if j.State == domain.JobRunning || j.State == domain.JobWaiting || j.State == domain.JobQueued {
			out.Running = append(out.Running, js)
		}
	}

	out.Files = s.rootsFor(r)

	// The resource view is best-effort: a console that cannot read /proc is
	// still a console, but it must say so rather than showing zeros as if the
	// machine had no memory.
	if s.Jobs.Guard() != nil {
		snap := s.Jobs.Guard().Sampler.Sample(0)
		if snap.MemErr != nil {
			out.ResourceError = "the machine's memory could not be read: " + snap.MemErr.Error()
		} else {
			v := &resourceView{
				MemAvailableKB:     snap.Mem.MemAvailableKB,
				MemTotalKB:         snap.Mem.MemTotalKB,
				SwapUsedKB:         snap.Mem.SwapTotalKB - snap.Mem.SwapFreeKB,
				SwapTotalKB:        snap.Mem.SwapTotalKB,
				WorkerRSSBytes:     snap.Worker.RSSBytes,
				PSIMemorySomeAvg10: snap.PSIMem.SomeAvg10,
				PSIIOSomeAvg10:     snap.PSIIO.SomeAvg10,
				Notes:              snap.Notes,
			}
			d := s.Jobs.Guard().BeforeFile()
			v.Verdict = d.Verdict.String()
			v.Reason = string(d.Reason)
			v.Detail = d.Detail
			out.Resources = v
			if d.Verdict != resource.Allow {
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"new heavy work would not start: %s", d.Detail))
			}
			// A memory warning is worth stating in absolute terms, because the
			// thresholds are what this machine was measured against.
			if lim := s.Jobs.Guard().Limits; lim.WarnMemAvailableKB > 0 &&
				snap.Mem.MemAvailableKB < lim.WarnMemAvailableKB {
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"available memory is %s, below the warning threshold of %s",
					kb(snap.Mem.MemAvailableKB), kb(lim.WarnMemAvailableKB)))
			}
		}
	} else {
		out.ResourceError = "no resource sampler is configured, so the machine's condition " +
			"cannot be reported"
	}

	out.ReadOnly = s.Jobs.ReadOnlyConsole() || principalOf(r.Context()).ReadOnlyUser()
	out.AllowAIHeavy = s.Jobs.AllowAIHeavy()
	out.Locks = lockNames(s.Jobs.HeldLocks())

	writeJSON(w, http.StatusOK, out)
}

// reasonSuffix renders a mode's reason for a warning line.
func reasonSuffix(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return ": " + reason
}

// kb renders a kibibyte count in a form a person reads.
func kb(v int64) string {
	if v >= 1024*1024 {
		return fmt.Sprintf("%.1f GiB", float64(v)/(1024*1024))
	}
	return fmt.Sprintf("%.0f MiB", float64(v)/1024)
}

// lockNames renders held locks as plain strings for the API.
func lockNames(m map[operations.MutexClass]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}

// handleOps publishes the operation registry: what the console can be asked to
// do, and what each thing requires.
func (s *Server) handleOps(w http.ResponseWriter, r *http.Request) {
	type paramDoc struct {
		Name     string   `json:"name"`
		Kind     string   `json:"kind"`
		Required bool     `json:"required"`
		MaxLen   int      `json:"max_len,omitempty"`
		Enum     []string `json:"enum,omitempty"`
		Doc      string   `json:"doc,omitempty"`
	}
	type opDoc struct {
		ID         string     `json:"id"`
		Summary    string     `json:"summary"`
		Scope      []string   `json:"scope"`
		Class      string     `json:"class"`
		Approval   string     `json:"approval"`
		Timeout    string     `json:"timeout"`
		Idempotent bool       `json:"idempotent"`
		Async      bool       `json:"async"`
		Mutex      string     `json:"mutex,omitempty"`
		ReadOnly   bool       `json:"read_only"`
		StopsWork  bool       `json:"stops_work,omitempty"`
		Params     []paramDoc `json:"params,omitempty"`
	}
	var out []opDoc
	for _, op := range s.Jobs.Registry().All() {
		d := opDoc{
			ID: op.ID, Summary: op.Summary, Scope: op.Scope,
			Class: string(op.Class), Approval: string(op.Approval),
			Timeout: op.Timeout.String(), Idempotent: op.Idempotent,
			Async: op.Async, Mutex: string(op.Mutex), ReadOnly: op.ReadOnly,
			StopsWork: op.StopsWork,
		}
		for _, p := range op.Params {
			d.Params = append(d.Params, paramDoc{
				Name: p.Name, Kind: string(p.Kind), Required: p.Required,
				MaxLen: p.MaxLen, Enum: p.Enum, Doc: p.Doc,
			})
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": out})
}

// handleAudit returns recent audited actions.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100)
	offset := intParam(r, "offset", 0)
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.Jobs.Store().Audit(r.Context(), limit, offset)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit, "offset": offset})
}

// handleModes returns the mode set.
func (s *Server) handleModes(w http.ResponseWriter, r *http.Request) {
	set, err := s.Jobs.Modes(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"states":    set.States,
		"effective": set.Effective(time.Now()),
	})
}

// setModeRequest is the body of a mode change.
type setModeRequest struct {
	Mode     string `json:"mode"`
	Duration string `json:"duration"`
	Reason   string `json:"reason"`
}

// handleSetMode sets a mode.
//
// Only watching, maintenance and manual_pause can be set through the API.
// resource_guard is the machine's own state and is not settable by a caller, so
// that a caller cannot silence a safety state or fabricate one.
func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	var req setModeRequest
	if err := decodeBody(w, r, &req); err != nil {
		return
	}
	var mode domain.Mode
	switch req.Mode {
	case string(domain.ModeWatching):
		mode = domain.ModeWatching
	case string(domain.ModeMaintenance):
		mode = domain.ModeMaintenance
	case string(domain.ModeManualPause):
		mode = domain.ModeManualPause
	case string(domain.ModeResourceGuard):
		writeError(w, http.StatusForbidden, "not_settable",
			"resource_guard is set by the machine when a resource cause is detected; "+
				"it is not set or cleared by a caller")
		return
	default:
		writeError(w, http.StatusBadRequest, "bad_request",
			"mode must be one of watching, maintenance, manual_pause")
		return
	}

	st := domain.ModeState{Mode: mode, Reason: req.Reason}
	switch mode {
	case domain.ModeManualPause:
		// A manual pause is durable: a human set it and a human clears it. An
		// expiring pause would let a forgotten one silently lift.
		if req.Duration != "" {
			writeError(w, http.StatusBadRequest, "bad_request",
				"a manual pause does not take a duration; it lasts until it is cleared")
			return
		}
	default:
		// A window must expire, so that a forgotten one cannot stop maintenance
		// forever.
		d, err := time.ParseDuration(req.Duration)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"a "+req.Mode+" window needs a positive duration such as \"2h\"")
			return
		}
		if d > 24*time.Hour {
			writeError(w, http.StatusBadRequest, "bad_request",
				"a "+req.Mode+" window may not exceed 24h")
			return
		}
		st.ExpiresAt = time.Now().Add(d)
	}

	p := s.principal(r)
	if err := s.Jobs.StartMode(r.Context(), st, p); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	set, err := s.Jobs.Modes(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"states": set.States, "effective": set.Effective(time.Now()),
	})
}

// handleClearMode clears a mode a person set.
func (s *Server) handleClearMode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("mode")
	mode := domain.Mode(name)
	switch mode {
	case domain.ModeWatching, domain.ModeMaintenance, domain.ModeManualPause:
	case domain.ModeResourceGuard:
		writeError(w, http.StatusForbidden, "not_settable",
			"resource_guard clears when the resource cause is resolved, not by request")
		return
	default:
		writeError(w, http.StatusBadRequest, "bad_request",
			"mode must be one of watching, maintenance, manual_pause")
		return
	}
	if err := s.Jobs.ClearMode(r.Context(), mode, s.principal(r)); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	set, err := s.Jobs.Modes(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"states": set.States, "effective": set.Effective(time.Now()),
	})
}

// handleApprovals lists outstanding approvals.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Jobs.Store().Approvals(r.Context(), intParam(r, "limit", 100))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": rows})
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

// rootDoc is one storage root as the UI sees it.
type rootDoc struct {
	ID            string          `json:"id"`
	Label         string          `json:"label"`
	Path          string          `json:"path"`
	ExposeHidden  bool            `json:"expose_hidden"`
	Available     bool            `json:"available"`
	Writable      bool            `json:"writable"`
	Note          string          `json:"note,omitempty"`
	Capacity      *files.Capacity `json:"capacity,omitempty"`
	CapacityError string          `json:"capacity_error,omitempty"`
}

type rootsResponse struct {
	Roots []rootDoc `json:"roots"`
	// Writable reports whether any available root permits operator writes under
	// the console's global readonly setting. Each root also reports its own flag.
	Writable       bool              `json:"writable"`
	Note           string            `json:"note,omitempty"`
	Capabilities   []string          `json:"capabilities"`
	UploadMaxBytes int64             `json:"upload_max_bytes"`
	Limits         config.FileLimits `json:"limits"`
}

// rootsInfo builds the roots response.
func (s *Server) rootsInfo() rootsResponse {
	out := rootsResponse{
		Roots:        []rootDoc{},
		Writable:     false,
		Note:         "storage roots default to read-only; writable roots support explicit operator actions",
		Capabilities: []string{"upload", "mkdir", "rename", "copy", "move", "trash", "restore", "purge", "cancel", "batch"},
	}
	if s.Files == nil {
		out.Note = "no storage roots are configured"
		return out
	}
	for _, r := range s.Files.Roots() {
		writable := r.Writable && r.Available && s.Jobs != nil && !s.Jobs.ReadOnlyConsole()
		if writable {
			out.Writable = true
		}
		out.Roots = append(out.Roots, rootDoc{
			ID: r.ID, Label: r.Label, Path: r.Path, ExposeHidden: r.ExposeHidden,
			Available: r.Available, Note: r.Note,
			Writable: writable,
			Capacity: r.Capacity, CapacityError: r.CapacityError,
		})
	}
	out.UploadMaxBytes = s.Files.Limits().UploadMaxBytes
	l := s.Files.Limits()
	out.Limits = config.FileLimits{MaxEntries: l.MaxEntries, MaxPageSize: l.MaxPageSize, DefaultPageSize: l.DefaultPageSize, SearchMaxResults: l.SearchMaxResults, SearchMaxExamined: l.SearchMaxExamined, PreviewMaxBytes: l.PreviewMaxBytes, UploadMaxBytes: l.UploadMaxBytes, TrashMaxEntries: l.TrashMaxEntries, CopyMaxEntries: l.CopyMaxEntries, CopyMaxBytes: l.CopyMaxBytes, BatchMaxItems: l.BatchMaxItems}
	if len(out.Roots) == 0 {
		out.Note = "no storage roots are configured; add [[hub.storage]] entries"
	}
	return out
}

func (s *Server) handleRoots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.rootsFor(r))
}

func (s *Server) rootsFor(r *http.Request) rootsResponse {
	out := s.rootsInfo()
	if principalOf(r.Context()).ReadOnlyUser() {
		out.Writable = false
		for i := range out.Roots {
			out.Roots[i].Writable = false
		}
		out.Capabilities = []string{}
	}
	return out
}

// entryDoc is one directory entry as the UI sees it.
type entryDoc struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
	Mode    string `json:"mode"`
	Symlink bool   `json:"symlink"`
	Kind    string `json:"kind"`
}

// listingResponse is one page of one directory.
type listingResponse struct {
	Root      string     `json:"root"`
	Path      string     `json:"path"`
	Parent    string     `json:"parent"`
	Entries   []entryDoc `json:"entries"`
	Page      int        `json:"page"`
	PageSize  int        `json:"page_size"`
	Total     int        `json:"total"`
	Pages     int        `json:"pages"`
	Truncated bool       `json:"truncated"`
	Sort      string     `json:"sort"`
	Order     string     `json:"order"`
	// Note explains a truncated listing in words.
	Note string `json:"note,omitempty"`
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage",
			"no storage roots are configured")
		return
	}
	page, size := s.pageParams(r)
	if r.URL.Query().Get("page_size") == "" {
		size = s.Files.Limits().DefaultPageSize
	}
	root := r.URL.Query().Get("root")
	if root == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "the root parameter is required")
		return
	}
	path := r.URL.Query().Get("path")
	l, err := s.Files.List(root, files.ListOptions{
		Path: path, Page: page, PageSize: size,
		Sort:  files.ParseSort(r.URL.Query().Get("sort")),
		Order: files.ParseOrder(r.URL.Query().Get("order")),
	})
	if err != nil {
		status, code := mapFileErr(err)
		s.fail(w, status, code, err)
		return
	}
	out := listingResponse{
		Root: l.Root, Path: l.Path, Parent: parentPath(l.Path),
		Entries: []entryDoc{}, Page: l.Page, PageSize: l.PageSize,
		Total: l.Total, Truncated: l.Truncated, Sort: l.Sort, Order: l.Order,
	}
	for _, e := range l.Entries {
		out.Entries = append(out.Entries, entryDoc{
			Name: e.Name, Path: e.Path, IsDir: e.IsDir, Size: e.Size,
			ModTime: e.ModTime.Format(time.RFC3339), Mode: e.Mode,
			Symlink: e.Symlink, Kind: e.Kind,
		})
	}
	if l.PageSize > 0 {
		out.Pages = (l.Total + l.PageSize - 1) / l.PageSize
	}
	if l.Truncated {
		out.Note = fmt.Sprintf("this directory holds more entries than the %d this "+
			"console examines at once, so the total and the page count are lower bounds",
			s.Files.Limits().MaxEntries)
	}
	writeJSON(w, http.StatusOK, out)
}

// parentPath renders the parent of a root-relative display path.
func parentPath(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func (s *Server) handleFileStat(w http.ResponseWriter, r *http.Request) {
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage",
			"no storage roots are configured")
		return
	}
	root, path := r.URL.Query().Get("root"), r.URL.Query().Get("path")
	if root == "" || path == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"the root and path parameters are required")
		return
	}
	e, err := s.Files.Stat(root, path)
	if err != nil {
		status, code := mapFileErr(err)
		s.fail(w, status, code, err)
		return
	}
	kind, previewable := files.PreviewKind(e.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"entry": entryDoc{
			Name: e.Name, Path: e.Path, IsDir: e.IsDir, Size: e.Size,
			ModTime: e.ModTime.Format(time.RFC3339), Mode: e.Mode,
			Symlink: e.Symlink, Kind: e.Kind,
		},
		"preview_kind": kind,
		"previewable":  previewable && !e.IsDir && (kind != "image" || e.Size <= min(s.Cfg.PreviewMaxBytes, s.Files.Limits().PreviewMaxBytes)),
	})
}

func (s *Server) handleFileSearch(w http.ResponseWriter, r *http.Request) {
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage",
			"no storage roots are configured")
		return
	}
	root := r.URL.Query().Get("root")
	q := r.URL.Query().Get("q")
	if root == "" || strings.TrimSpace(q) == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"the root and q parameters are required")
		return
	}
	res, err := s.Files.Search(root, files.SearchOptions{
		Path:     r.URL.Query().Get("path"),
		Query:    q,
		MaxDepth: intParam(r, "depth", 2),
	})
	if err != nil {
		status, code := mapFileErr(err)
		s.fail(w, status, code, err)
		return
	}
	out := map[string]any{
		"root": res.Root, "entries": []entryDoc{}, "examined": res.Examined,
		"truncated": res.Truncated, "elapsed_ms": res.ElapsedMS,
	}
	entries := make([]entryDoc, 0, len(res.Entries))
	for _, e := range res.Entries {
		entries = append(entries, entryDoc{
			Name: e.Name, Path: e.Path, IsDir: e.IsDir, Size: e.Size,
			ModTime: e.ModTime.Format(time.RFC3339), Mode: e.Mode,
			Symlink: e.Symlink, Kind: e.Kind,
		})
	}
	out["entries"] = entries
	if res.Truncated {
		out["note"] = "the search stopped at its " + res.Reason + "; more matches may exist"
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFilePreview streams a file for in-browser display.
//
// The console does not decode, resize or transcode anything here. It passes the
// bytes through, which is why previewing a directory of large photographs costs
// this process almost nothing. Files larger than the configured preview cap are
// refused with an explanation rather than streamed, so a request cannot be used
// to pull a 4 GiB file through a page.
func (s *Server) handleFilePreview(w http.ResponseWriter, r *http.Request) {
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage",
			"no storage roots are configured")
		return
	}
	root, path := r.URL.Query().Get("root"), r.URL.Query().Get("path")
	if root == "" || path == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"the root and path parameters are required")
		return
	}

	select {
	case s.streamSlots <- struct{}{}:
		defer func() { <-s.streamSlots }()
	case <-r.Context().Done():
		return
	default:
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusServiceUnavailable, "busy",
			"too many transfers are in progress; try again shortly")
		return
	}

	f, fi, err := s.Files.Open(root, path)
	if err != nil {
		status, code := mapFileErr(err)
		s.fail(w, status, code, err)
		return
	}
	defer f.Close()

	kind, ok := files.PreviewKind(fi.Name())
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, "no_preview",
			"this file type has no in-browser preview; download it instead")
		return
	}

	// The size cap applies to images only, and that distinction is deliberate.
	//
	// An image is sent whole: the browser cannot render half a JPEG, so a
	// request for a very large image would pull the entire thing through the
	// page. A 32 MiB image is already unreasonable, so it is refused with an
	// explanation rather than streamed.
	//
	// Video and audio are different, because they are ranged. The browser asks
	// for the byte ranges it needs to play and seek, so a 4 GiB film costs this
	// process a bounded buffer per request rather than the whole file - and
	// capping it at 32 MiB would make video playback impossible, which is the
	// feature the cap would be breaking.
	previewLimit := min(s.Cfg.PreviewMaxBytes, s.Files.Limits().PreviewMaxBytes)
	if kind == "image" && fi.Size() > previewLimit {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("this image is %s; inline preview is limited to %s. "+
				"Download it instead, or raise the limit in the configuration",
				kb(fi.Size()/1024), kb(previewLimit/1024)))
		return
	}

	// A content type derived from the extension, with nosniff already set, is
	// enough here: the bytes come from the operator's own storage, and the
	// browser is the only decoder involved.
	w.Header().Set("Content-Type", contentTypeFor(fi.Name()))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", "inline")
	// A private cache so that moving between entries in the UI does not
	// re-transfer the same image.
	w.Header().Set("Cache-Control", "private, max-age=300")

	// Range support matters most for video. Without it, seeking in a file the
	// browser cannot stream from the start means re-transferring from byte zero,
	// which on a NAS means pulling gigabytes over a tailnet to skip a minute.
	// http.ServeContent implements the ranges, the 206 responses, the
	// If-Range/If-Modified-Since handling and the multi-range multipart encoding
	// correctly; hand-rolling that is how a player ends up with a subtly wrong
	// Content-Range and a stuck spinner.
	//
	// It also means the console never buffers the whole file: ServeContent
	// copies through the same bounded path.
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
	_ = kind
}

// contentTypeFor maps a filename to a content type for in-browser display.
func contentTypeFor(name string) string {
	switch strings.ToLower(name[strings.LastIndex(name, ".")+1:]) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "bmp":
		return "image/bmp"
	case "avif":
		return "image/avif"
	case "mp4", "m4v":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mov":
		return "video/quicktime"
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "m4a":
		return "audio/mp4"
	case "ogg", "opus":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "txt", "nfo", "srt", "ass", "sub", "md":
		return "text/plain; charset=utf-8"
	case "json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 50)
	offset := intParam(r, "offset", 0)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	f := hubstore.JobFilter{
		Limit: limit, Offset: offset,
		Op:   r.URL.Query().Get("op"),
		Open: boolParam(r, "open"),
	}
	p := principalOf(r.Context())
	if p.Kind == domain.PrincipalUser && !p.DeviceAdmin() {
		f.Principal = p.String()
	}
	if st := r.URL.Query().Get("state"); st != "" {
		if !domain.JobState(st).Valid() {
			writeError(w, http.StatusBadRequest, "bad_request",
				"state must be one of queued, running, waiting, paused, completed, "+
					"failed, cancelled, interrupted, unknown")
			return
		}
		f.State = domain.JobState(st)
	}
	rows, err := s.Jobs.Store().Jobs(r.Context(), f)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	out := make([]jobSummary, 0, len(rows))
	for _, j := range rows {
		summary := toJobSummary(j)
		summary.Cancellable = !j.State.Terminal() && canStopJob(principalOf(r.Context()), j) && s.canCancel(j.ID)
		out = append(out, summary)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": out, "limit": limit, "offset": offset,
	})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > 64 {
		writeError(w, http.StatusBadRequest, "bad_request", "a job id is required")
		return
	}
	j, err := s.Jobs.Store().Job(r.Context(), id)
	if err != nil {
		if errors.Is(err, hubstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such job")
			return
		}
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	if !canReadJob(principalOf(r.Context()), *j) {
		writeError(w, 404, "not_found", "no such job")
		return
	}
	events, err := s.Jobs.Store().JobEvents(r.Context(), id, 200)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	summary := toJobSummary(*j)
	summary.Cancellable = !j.State.Terminal() && canStopJob(principalOf(r.Context()), *j) && s.canCancel(j.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"job": summary, "events": events,
	})
}

// boolParam reads a boolean query parameter.
func boolParam(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Media (IWantWatchAnime)
// ---------------------------------------------------------------------------

// mediaStatusResponse reports the integration's state. It is a read of suzu's own
// database; the console never writes it.
type mediaStatusResponse struct {
	Configured bool     `json:"configured"`
	DBPath     string   `json:"db_path,omitempty"`
	APIBase    string   `json:"api_base,omitempty"`
	Note       string   `json:"note"`
	Tables     []string `json:"tables,omitempty"`

	LibraryCount      *int `json:"library_count,omitempty"`
	SubscriptionCount *int `json:"subscription_count,omitempty"`
	TaskCount         *int `json:"task_count,omitempty"`

	Library []suzu.LibraryItem `json:"library,omitempty"`
	Tasks   []suzu.Task        `json:"tasks,omitempty"`
}

func (s *Server) handleMediaStatus(w http.ResponseWriter, r *http.Request) {
	if s.Suzu == nil || !s.Suzu.Configured() {
		writeJSON(w, http.StatusOK, mediaStatusResponse{
			Configured: false,
			Note: "the IWantWatchAnime integration is not configured. Set hub.suzu.db_path " +
				"in the console's configuration to enable it. Until then the console " +
				"reports nothing about the library rather than guessing at it.",
		})
		return
	}
	ctx, cancel := contextWithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	st := s.Suzu.Status(ctx)
	out := mediaStatusResponse{
		Configured:        st.Configured,
		DBPath:            st.DBPath,
		APIBase:           st.APIBase,
		Note:              st.Note,
		Tables:            st.Tables,
		LibraryCount:      st.LibraryCount,
		SubscriptionCount: st.SubscriptionCount,
		TaskCount:         st.TaskCount,
	}
	if items, _, err := s.Suzu.Library(ctx, r.URL.Query().Get("q"),
		intParam(r, "limit", 50), intParam(r, "offset", 0)); err == nil {
		out.Library = items
	} else {
		out.Note += "; the library could not be read: " + err.Error()
	}
	if tasks, err := s.Suzu.Tasks(ctx); err == nil {
		out.Tasks = tasks
	}
	writeJSON(w, http.StatusOK, out)
}
