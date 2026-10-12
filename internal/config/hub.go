package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// Hub configures the unified console (`nasctl`). It is a separate section from
// the album configuration because the console is a separate process with a
// different job: it reads, it renders and it starts *approved* work. The album
// settings keep governing the album, and nothing here can widen them.
//
// Every default is closed. An operator who copies the example config and starts
// the console gets a read-only console with no storage roots and no integrations,
// which is the only safe thing to get by accident.
type Hub struct {
	// Bind is the console's listen address. It is separate from the album
	// service's bind because the two are different processes with different
	// jobs: the console must be able to sit on the tailnet while the album
	// service stays on loopback, or the other way round.
	//
	// Empty means "use server.bind", so an operator who has already set a
	// private address there does not have to repeat it.
	Bind string `toml:"bind"`

	// Storage lists the directories the file module may browse. They are
	// addressed by ID, never by path: a request carries "photos", not
	// "/mnt/data/photos", so no request can name a directory the operator did
	// not configure.
	Storage []HubStorage `toml:"storage"`

	// Suzu describes the existing IWantWatchAnime service. The console reads its
	// database and calls its API; it never writes that database, because suzu is
	// the only writer of its own state.
	Suzu Suzu `toml:"suzu"`

	// Agents lists credentials that identify an AI principal. This is what makes
	// the agent distinction real rather than advisory.
	//
	// Without it, an agent would be recognised only by a header it declares about
	// itself, so it could escape every agent restriction by simply not sending
	// one. With it, the credential decides the principal: a caller holding an
	// agent token *is* that agent, whatever it claims, and a caller holding only
	// the shared token is the operator.
	Agents []Agent `toml:"agents"`

	// Jobs bounds the console's own job records.
	Jobs JobLimits `toml:"jobs"`
}

// Agent is one AI principal's credential.
type Agent struct {
	// Name identifies the agent in the audit log and in approvals, e.g. "hermes".
	Name string `toml:"name"`
	// Token is the credential it presents. It must differ from server.token and
	// from every other agent's, or the identity would be ambiguous.
	Token string `toml:"token"`
	// AllowHeavy lets this agent start bounded heavy work. It is per-agent so
	// that one trusted agent can be enabled without enabling all of them.
	AllowHeavy bool `toml:"allow_heavy"`
}

// HubStorage is one browsable root.
type HubStorage struct {
	ID    string `toml:"id"`
	Label string `toml:"label"`
	Path  string `toml:"path"`

	// Writable is refused while it is true. File writes (upload, mkdir, rename,
	// move, copy, trash) are stage E work: the code does not exist yet, so
	// accepting this flag would promise a capability the binary does not have.
	// The key is parsed rather than rejected outright so that a config written
	// for a later version produces a clear message instead of a typo report.
	Writable bool `toml:"writable"`

	// ExposeHidden includes dot-prefixed entries in listings. Off by default:
	// hidden files on a NAS are usually tool state (.tmp, .stfolder, @eaDir),
	// and showing them invites an operator to delete something a tool needs.
	ExposeHidden bool `toml:"expose_hidden"`
}

// Suzu configures the read-only integration with IWantWatchAnime.
type Suzu struct {
	// DBPath is suzu's SQLite file. Opened read-only.
	DBPath string `toml:"db_path"`

	// APIBase is suzu's own HTTP endpoint, used for liveness and - in a later
	// stage - for writes through its API. Must be a loopback address.
	APIBase string `toml:"api_base"`

	// Token is sent as X-Api-Token when calling suzu. Required only for writes,
	// which this stage does not perform.
	Token string `toml:"token"`

	// LibraryRoot is where suzu archives episodes. Used to resolve a library
	// row's stored path to a file the console may stream. When empty, playback
	// is disabled and the console says so instead of guessing.
	LibraryRoot string `toml:"library_root"`
}

// JobLimits bounds the console's own bookkeeping.
type JobLimits struct {
	// MaxOpenJobs caps queued+running jobs. A console on a 981 MiB machine
	// should never have many, and a cap makes "the queue grew without bound"
	// impossible rather than unlikely.
	MaxOpenJobs int `toml:"max_open_jobs"`

	// MaxHistory caps finished job rows; older ones are pruned by the console
	// itself. Bounded growth, like everything else that writes.
	MaxHistory int `toml:"max_history"`

	// AuditRetentionDays bounds the audit log.
	AuditRetentionDays int `toml:"audit_retention_days"`
}

// storageIDPattern keeps IDs to characters that survive a URL path segment
// without escaping, so that an ID can never be confused with a path.
var storageIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// finalizeHub applies defaults and validates the hub section. It is called from
// Finalize so that a single validation pass covers the whole file, and so that
// an unknown or contradictory hub key is reported by the same mechanism as
// every other setting.
func (c *Config) finalizeHub(fail func(string, ...any)) {
	h := &c.Hub
	serverToken := c.Server.Token

	if h.Jobs.MaxOpenJobs <= 0 {
		h.Jobs.MaxOpenJobs = 32
	}
	if h.Jobs.MaxOpenJobs > 1000 {
		fail("hub.jobs.max_open_jobs (%d) is above 1000; a console that queues more than "+
			"that on this machine is a console that has lost track of its own state",
			h.Jobs.MaxOpenJobs)
	}
	if h.Jobs.MaxHistory <= 0 {
		h.Jobs.MaxHistory = 5000
	}
	if h.Jobs.AuditRetentionDays <= 0 {
		h.Jobs.AuditRetentionDays = 90
	}

	// ---- agents ----
	seenAgent := map[string]int{}
	seenToken := map[string]string{}
	if serverToken != "" {
		seenToken[serverToken] = "server.token"
	}
	for i, a := range h.Agents {
		where := fmt.Sprintf("hub.agents[%d]", i)
		if strings.TrimSpace(a.Name) == "" {
			fail("%s.name is empty; an agent without a name cannot be identified in the "+
				"audit log or bound by an approval", where)
			continue
		}
		if !storageIDPattern.MatchString(a.Name) {
			fail("%s.name %q must be lowercase letters, digits, '-' or '_' and at most 32 "+
				"characters: it appears in the audit log and in approval records", where, a.Name)
		}
		if prev, dup := seenAgent[a.Name]; dup {
			fail("%s.name %q is already used by hub.agents[%d]", where, a.Name, prev)
		}
		seenAgent[a.Name] = i

		if strings.TrimSpace(a.Token) == "" {
			fail("%s.token is empty, so this agent could never authenticate; an agent "+
				"without a credential is an agent that cannot be told apart from the "+
				"operator, which defeats the reason for listing it", where)
		} else if len(a.Token) < 16 {
			fail("%s.token is only %d characters; an agent credential that is short is one "+
				"that can be guessed, and it is the only thing separating an agent from "+
				"the operator", where, len(a.Token))
		}
		if prev, dup := seenToken[a.Token]; dup {
			fail("%s.token is the same as %s; each credential must identify exactly one "+
				"principal, or the console cannot tell who is calling", where, prev)
		}
		seenToken[a.Token] = where + ".token"
	}

	seen := map[string]int{}
	for i, s := range h.Storage {
		where := fmt.Sprintf("hub.storage[%d]", i)
		if s.ID == "" {
			fail("%s.id is empty; a root without an ID cannot be addressed by a request", where)
			continue
		}
		if !storageIDPattern.MatchString(s.ID) {
			fail("%s.id %q must be lowercase letters, digits, '-' or '_' and at most 32 "+
				"characters: it appears in URL paths and must never look like a path",
				where, s.ID)
		}
		if prev, dup := seen[s.ID]; dup {
			fail("%s.id %q is already used by hub.storage[%d]; IDs address roots, so they "+
				"must be unique", where, s.ID, prev)
		}
		seen[s.ID] = i

		if strings.TrimSpace(s.Path) == "" {
			fail("%s.path is empty", where)
		} else if !filepath.IsAbs(s.Path) {
			fail("%s.path %q must be absolute: a relative root would depend on the working "+
				"directory of the console, which is not something a request may influence",
				where, s.Path)
		} else if cleaned := filepath.Clean(s.Path); cleaned != s.Path {
			fail("%s.path %q is not clean (did you mean %q?)", where, s.Path, cleaned)
		}
		if s.Writable {
			fail("%s.writable = true is refused: file writes are not implemented in this "+
				"version, so this flag would claim a capability the binary does not have. "+
				"Leave it false until upload/rename/trash are delivered and accepted", where)
		}
	}

	sz := &h.Suzu
	if sz.DBPath != "" {
		if !filepath.IsAbs(sz.DBPath) {
			fail("hub.suzu.db_path %q must be absolute", sz.DBPath)
		}
		if _, err := c.ensureOutsideMedia("hub.suzu.db_path", sz.DBPath); err != nil {
			fail("%v", err)
		}
	}
	if sz.LibraryRoot != "" {
		if !filepath.IsAbs(sz.LibraryRoot) {
			fail("hub.suzu.library_root %q must be absolute", sz.LibraryRoot)
		}
	}
	if sz.APIBase != "" {
		u, err := url.Parse(sz.APIBase)
		if err != nil {
			fail("hub.suzu.api_base %q is not a URL: %v", sz.APIBase, err)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			fail("hub.suzu.api_base %q must be http or https", sz.APIBase)
		} else {
			host := u.Hostname()
			if ip := net.ParseIP(host); ip != nil {
				if !ip.IsLoopback() {
					fail("hub.suzu.api_base %q must be a loopback address: the console talks "+
						"to a service on this machine, and a remote endpoint would mean the "+
						"console is sending a credential over the network", sz.APIBase)
				}
			} else if host != "localhost" {
				fail("hub.suzu.api_base %q must be a loopback address or localhost", sz.APIBase)
			}
		}
	}
}

// ensureOutsideMedia reports an error when a hub-owned path sits inside one of
// the configured media trees. The console writes its own state; a state file
// inside a browsable, backed-up media root would be both indexed as media and
// copied to the cloud, which is the mistake this check exists to prevent.
func (c *Config) ensureOutsideMedia(what, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	for _, col := range c.Collections {
		if pathContains(col.Root, abs) {
			return "", fmt.Errorf("%s %q is inside collection %q's root %q; hub state must "+
				"not live in a media tree, or it will be indexed as media and copied by "+
				"the backup", what, abs, col.Code, col.Root)
		}
	}
	for _, s := range c.Hub.Storage {
		if pathContains(s.Path, abs) {
			return "", fmt.Errorf("%s %q is inside hub.storage %q's path %q; hub state must "+
				"not live in a browsable tree", what, abs, s.ID, s.Path)
		}
	}
	return abs, nil
}
