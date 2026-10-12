// Package hubops declares every operation the console offers, with its scope,
// risk class, approval rule, timeout, idempotency and mutex.
//
// This file is the permission table. Reading it top to bottom tells an operator
// exactly what the console can do, to what, and what it takes to do it - without
// reading any handler. That property is the point: a permission model spread
// through implementations cannot be audited, and cannot be reviewed by anyone who
// is not the implementer.
//
// Operations declared here without a handler are still declared. The console
// reports "declared, not implemented in this build" rather than silently
// omitting the operation, so a caller can tell the difference between "you may
// not" and "not yet".
package hubops

import (
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

// Declarations returns the full registry.
func Declarations() []operations.Operation {
	return []operations.Operation{
		// -----------------------------------------------------------------
		// System: read-only facts about the machine and the console.
		// -----------------------------------------------------------------
		{
			ID: "system.status", Summary: "Machine and console status",
			Scope: []string{"system"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "system.mounts", Summary: "Mounted filesystems and their identity",
			Scope: []string{"system"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "system.services", Summary: "State of the services this console reads",
			Scope: []string{"system"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 8 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "unit", Kind: operations.ParamString, MaxLen: 128,
					Doc: "restrict the report to one unit"},
			},
		},
		{
			ID: "system.audit", Summary: "Recent audited actions",
			Scope: []string{"system"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "limit", Kind: operations.ParamInt, Doc: "rows to return, capped"},
				{Name: "offset", Kind: operations.ParamInt, Doc: "rows to skip"},
			},
		},

		// -----------------------------------------------------------------
		// Files: our own file manager. Reads are open; nothing that writes
		// exists yet, and the declarations below are the shape those
		// operations will take when they are delivered.
		// -----------------------------------------------------------------
		{
			ID: "files.roots", Summary: "List configured storage roots",
			Scope: []string{"storage"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "files.list", Summary: "List one directory level",
			Scope: []string{"storage"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 10 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32,
					Doc: "storage root ID"},
				{Name: "path", Kind: operations.ParamString, MaxLen: 1024,
					Doc: "root-relative path; '/' or empty for the root itself"},
				{Name: "page", Kind: operations.ParamInt, Doc: "1-based page"},
				{Name: "page_size", Kind: operations.ParamInt, Doc: "entries per page, capped"},
				{Name: "sort", Kind: operations.ParamEnum, Enum: []string{"name", "size", "time", "kind"},
					Doc: "sort key"},
				{Name: "order", Kind: operations.ParamEnum, Enum: []string{"asc", "desc"},
					Doc: "sort direction"},
			},
		},
		{
			ID: "files.stat", Summary: "Metadata for one entry",
			Scope: []string{"storage"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "path", Kind: operations.ParamString, Required: true, MaxLen: 1024},
			},
		},
		{
			ID: "files.search", Summary: "Bounded search for a name",
			Scope: []string{"storage"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 15 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "path", Kind: operations.ParamString, MaxLen: 1024},
				{Name: "q", Kind: operations.ParamString, Required: true, MaxLen: 128},
				{Name: "depth", Kind: operations.ParamInt, Doc: "levels to descend, capped"},
			},
		},

		// Writes. Declared so that the permission model is visible and so that a
		// caller gets "not implemented" rather than "unknown operation". They
		// are refused by the console in every configuration until the code
		// exists, because a declared operation with no handler fails closed.
		{
			ID: "files.upload", Summary: "Upload a file into a storage root (not implemented)",
			Scope: []string{"storage"}, Class: domain.RiskMutating, Approval: domain.ApprovalPerCall,
			Timeout: 30 * time.Minute, Async: true, Mutex: operations.MutexMedia,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "path", Kind: operations.ParamString, Required: true, MaxLen: 1024},
				{Name: "size", Kind: operations.ParamInt, Doc: "declared size, checked against the stream"},
				{Name: "overwrite", Kind: operations.ParamEnum, Enum: []string{"", "refuse", "replace"},
					Doc: "defaults to refuse"},
			},
		},
		{
			ID: "files.mkdir", Summary: "Create a directory (not implemented)",
			Scope: []string{"storage"}, Class: domain.RiskMutating, Approval: domain.ApprovalPerCall,
			Timeout: 30 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "path", Kind: operations.ParamString, Required: true, MaxLen: 1024},
			},
		},
		{
			ID: "files.rename", Summary: "Rename an entry (not implemented)",
			Scope: []string{"storage"}, Class: domain.RiskMutating, Approval: domain.ApprovalPerCall,
			Timeout: 60 * time.Second,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "from", Kind: operations.ParamString, Required: true, MaxLen: 1024},
				{Name: "to", Kind: operations.ParamString, Required: true, MaxLen: 1024},
			},
		},
		{
			ID: "files.copy", Summary: "Copy files as a bounded background job (not implemented)",
			Scope: []string{"storage"}, Class: domain.RiskMutating, Approval: domain.ApprovalPerCall,
			Timeout: 2 * time.Hour, Async: true, Mutex: operations.MutexMedia,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "from", Kind: operations.ParamString, Required: true, MaxLen: 1024},
				{Name: "to", Kind: operations.ParamString, Required: true, MaxLen: 1024},
				{Name: "limit", Kind: operations.ParamInt, Doc: "maximum files to copy"},
			},
		},
		{
			ID: "files.trash", Summary: "Move entries to a bounded trash (not implemented)",
			Scope: []string{"storage"}, Class: domain.RiskMutating, Approval: domain.ApprovalPerCall,
			Timeout: 5 * time.Minute, Async: true,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "path", Kind: operations.ParamString, Required: true, MaxLen: 1024},
			},
		},
		{
			ID: "files.restore", Summary: "Restore entries from the trash (not implemented)",
			Scope: []string{"storage"}, Class: domain.RiskMutating, Approval: domain.ApprovalPerCall,
			Timeout: 5 * time.Minute, Async: true,
			Params: []operations.Param{
				{Name: "root", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "id", Kind: operations.ParamString, Required: true, MaxLen: 64},
			},
		},

		// -----------------------------------------------------------------
		// Album: reuses albumctl's bounded, bounded-again operations.
		// -----------------------------------------------------------------
		{
			ID: "album.collections", Summary: "List configured album collections",
			Scope: []string{"collection"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "album.timeline", Summary: "Paged timeline of indexed media",
			Scope: []string{"collection"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 10 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "collection", Kind: operations.ParamString, MaxLen: 32},
				{Name: "from", Kind: operations.ParamString, MaxLen: 32, Doc: "inclusive date"},
				{Name: "to", Kind: operations.ParamString, MaxLen: 32, Doc: "inclusive date"},
				{Name: "q", Kind: operations.ParamString, MaxLen: 128},
				{Name: "page", Kind: operations.ParamInt},
				{Name: "page_size", Kind: operations.ParamInt},
			},
		},
		{
			ID: "album.media", Summary: "Metadata for one indexed media item",
			Scope: []string{"collection"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "id", Kind: operations.ParamInt, Required: true},
			},
		},
		{
			ID: "album.scan", Summary: "Bounded directory and metadata scan",
			Scope: []string{"collection"}, Class: domain.RiskBounded, Approval: domain.ApprovalPreauthorised,
			Timeout: 2 * time.Hour, Async: true, Mutex: operations.MutexScan,
			Params: []operations.Param{
				{Name: "collection", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "limit", Kind: operations.ParamInt, Required: true,
					Doc: "maximum files this run may index; required, so a run cannot be unbounded"},
				{Name: "dry_run", Kind: operations.ParamBool, Doc: "preview only"},
			},
		},
		{
			ID: "album.thumbs", Summary: "Bounded thumbnail generation batch",
			Scope: []string{"collection"}, Class: domain.RiskBounded, Approval: domain.ApprovalPreauthorised,
			Timeout: 2 * time.Hour, Async: true, Mutex: operations.MutexAlbum,
			Params: []operations.Param{
				{Name: "collection", Kind: operations.ParamString, Required: true, MaxLen: 32},
				{Name: "limit", Kind: operations.ParamInt, Required: true},
				{Name: "max_runtime", Kind: operations.ParamString, MaxLen: 16,
					Doc: "wall-clock bound, e.g. 20m"},
			},
		},
		{
			ID: "album.pause", Summary: "Pause album heavy work durably",
			Scope: []string{"album"}, Class: domain.RiskBounded, Approval: domain.ApprovalPreauthorised,
			Timeout: 10 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "reason", Kind: operations.ParamString, MaxLen: 200},
			},
		},
		{
			ID: "album.resume", Summary: "Clear a pause, without starting any work",
			Scope: []string{"album"}, Class: domain.RiskBounded, Approval: domain.ApprovalPreauthorised,
			Timeout: 10 * time.Second, Idempotent: true,
		},
		{
			ID: "album.cache_report", Summary: "Thumbnail cache usage and hit counts",
			Scope: []string{"album"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 30 * time.Second, Idempotent: true, ReadOnly: true,
		},

		// -----------------------------------------------------------------
		// Media: the IWantWatchAnime integration.
		// -----------------------------------------------------------------
		{
			ID: "media.library", Summary: "Paged list of archived episodes",
			Scope: []string{"suzu"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 10 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "page", Kind: operations.ParamInt},
				{Name: "page_size", Kind: operations.ParamInt},
				{Name: "q", Kind: operations.ParamString, MaxLen: 128},
			},
		},
		{
			ID: "media.subscriptions", Summary: "Paged list of subscriptions",
			Scope: []string{"suzu"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 10 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "page", Kind: operations.ParamInt},
				{Name: "page_size", Kind: operations.ParamInt},
			},
		},
		{
			ID: "media.tasks", Summary: "Download tasks suzu is running",
			Scope: []string{"suzu"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 10 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "media.progress", Summary: "Record watch progress for an item",
			Scope: []string{"suzu"}, Class: domain.RiskMutating, Approval: domain.ApprovalPreauthorised,
			Timeout: 10 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "item", Kind: operations.ParamString, Required: true, MaxLen: 64},
				{Name: "position", Kind: operations.ParamInt, Doc: "seconds watched"},
				{Name: "duration", Kind: operations.ParamInt, Doc: "total seconds"},
			},
		},
		{
			ID: "media.favourite", Summary: "Mark or unmark an item as a favourite",
			Scope: []string{"suzu"}, Class: domain.RiskMutating, Approval: domain.ApprovalPreauthorised,
			Timeout: 10 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "item", Kind: operations.ParamString, Required: true, MaxLen: 64},
				{Name: "on", Kind: operations.ParamBool},
			},
		},

		// -----------------------------------------------------------------
		// Backup: read-only status. The console never starts, stops or
		// reconfigures the existing backup; it reports on it and yields to it.
		// -----------------------------------------------------------------
		{
			ID: "backup.status", Summary: "Backup service state and recent activity",
			Scope: []string{"backup"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 10 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "backup.plan", Summary: "What the backup intends to transfer, as reported",
			Scope: []string{"backup"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 10 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "backup.verify", Summary: "Last verification result, if one is recorded",
			Scope: []string{"backup"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 10 * time.Second, Idempotent: true, ReadOnly: true,
		},

		// -----------------------------------------------------------------
		// Jobs: the console's own task control.
		// -----------------------------------------------------------------
		{
			ID: "jobs.list", Summary: "Paged job history",
			Scope: []string{"job"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "state", Kind: operations.ParamString, MaxLen: 24},
				{Name: "op", Kind: operations.ParamString, MaxLen: 64},
				{Name: "open", Kind: operations.ParamBool},
				{Name: "limit", Kind: operations.ParamInt},
				{Name: "offset", Kind: operations.ParamInt},
			},
		},
		{
			ID: "jobs.get", Summary: "One job with its event history",
			Scope: []string{"job"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
			Params: []operations.Param{
				{Name: "id", Kind: operations.ParamString, Required: true, MaxLen: 64},
			},
		},
		{
			ID: "jobs.cancel", Summary: "Cancel a job this console started",
			Scope: []string{"job"}, Class: domain.RiskBounded, Approval: domain.ApprovalPreauthorised,
			Timeout: 30 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "id", Kind: operations.ParamString, Required: true, MaxLen: 64},
			},
		},

		// -----------------------------------------------------------------
		// Modes and approvals.
		// -----------------------------------------------------------------
		{
			ID: "mode.get", Summary: "Current mode set and its sources",
			Scope: []string{"system"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "mode.set_watching", Summary: "Enter a bounded watching window",
			Scope: []string{"system"}, Class: domain.RiskBounded, Approval: domain.ApprovalPreauthorised,
			Timeout: 10 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "duration", Kind: operations.ParamString, Required: true, MaxLen: 16,
					Doc: "e.g. 2h; a watching window always expires"},
				{Name: "reason", Kind: operations.ParamString, MaxLen: 200},
			},
		},
		{
			ID: "mode.set_maintenance", Summary: "Enter a bounded maintenance window",
			Scope: []string{"system"}, Class: domain.RiskBounded, Approval: domain.ApprovalPreauthorised,
			Timeout: 10 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "duration", Kind: operations.ParamString, Required: true, MaxLen: 16},
				{Name: "reason", Kind: operations.ParamString, MaxLen: 200},
			},
		},
		{
			ID: "mode.clear", Summary: "Clear a mode a person set",
			Scope: []string{"system"}, Class: domain.RiskBounded, Approval: domain.ApprovalPreauthorised,
			Timeout: 10 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "mode", Kind: operations.ParamEnum, Required: true,
					Enum: []string{"watching", "maintenance", "manual_pause"}},
			},
		},
		{
			ID: "approval.list", Summary: "Outstanding approvals",
			Scope: []string{"system"}, Class: domain.RiskRead, Approval: domain.ApprovalNone,
			Timeout: 5 * time.Second, Idempotent: true, ReadOnly: true,
		},
		{
			ID: "approval.grant", Summary: "Grant a scoped, expiring approval",
			Scope: []string{"system"}, Class: domain.RiskPrivileged, Approval: domain.ApprovalPerCall,
			Timeout: 10 * time.Second,
			Params: []operations.Param{
				{Name: "op", Kind: operations.ParamString, Required: true, MaxLen: 64},
				{Name: "for", Kind: operations.ParamString, Required: true, MaxLen: 64,
					Doc: "principal the approval is for, e.g. ai:hermes"},
				{Name: "target", Kind: operations.ParamString, MaxLen: 128},
				{Name: "params", Kind: operations.ParamString, MaxLen: 256,
					Doc: "exact parameter string the approval covers"},
				{Name: "duration", Kind: operations.ParamString, Required: true, MaxLen: 16},
				{Name: "one_shot", Kind: operations.ParamBool},
			},
		},
		{
			ID: "approval.revoke", Summary: "Revoke an approval",
			Scope: []string{"system"}, Class: domain.RiskPrivileged, Approval: domain.ApprovalPerCall,
			Timeout: 10 * time.Second, Idempotent: true,
			Params: []operations.Param{
				{Name: "id", Kind: operations.ParamString, Required: true, MaxLen: 64},
			},
		},

		// -----------------------------------------------------------------
		// Scheduling: declared, not implemented. Enabling automation is an
		// operator decision that must not be reachable by accident.
		// -----------------------------------------------------------------
		{
			ID: "schedule.enable", Summary: "Enable automatic scheduling (not implemented)",
			Scope: []string{"system"}, Class: domain.RiskPrivileged, Approval: domain.ApprovalPerCall,
			Timeout: 30 * time.Second,
			Params: []operations.Param{
				{Name: "window", Kind: operations.ParamString, Required: true, MaxLen: 64},
				{Name: "max_files", Kind: operations.ParamInt},
				{Name: "max_minutes", Kind: operations.ParamInt},
			},
		},
		{
			ID: "schedule.disable", Summary: "Disable automatic scheduling",
			Scope: []string{"system"}, Class: domain.RiskPrivileged, Approval: domain.ApprovalPerCall,
			Timeout: 30 * time.Second, Idempotent: true,
		},
	}
}
