package config

// MaxFileTransferBytes bounds operator-configured large NAS transfers without
// making memory use proportional to the transfer size. Defaults stay smaller.
const MaxFileTransferBytes int64 = 1 << 40

// FileLimits are local operator policy. Requests can lower these budgets but
// cannot raise them. Zero selects the default; negative values are errors.
type FileLimits struct {
	MaxEntries        int   `toml:"max_entries" json:"max_entries"`
	MaxPageSize       int   `toml:"max_page_size" json:"max_page_size"`
	DefaultPageSize   int   `toml:"default_page_size" json:"default_page_size"`
	SearchMaxResults  int   `toml:"search_max_results" json:"search_max_results"`
	SearchMaxExamined int   `toml:"search_max_examined" json:"search_max_examined"`
	PreviewMaxBytes   int64 `toml:"preview_max_bytes" json:"preview_max_bytes"`
	UploadMaxBytes    int64 `toml:"upload_max_bytes" json:"upload_max_bytes"`
	TrashMaxEntries   int   `toml:"trash_max_entries" json:"trash_max_entries"`
	CopyMaxEntries    int   `toml:"copy_max_entries" json:"copy_max_entries"`
	CopyMaxBytes      int64 `toml:"copy_max_bytes" json:"copy_max_bytes"`
	BatchMaxItems     int   `toml:"batch_max_items" json:"batch_max_items"`
}

func DefaultFileLimits() FileLimits {
	return FileLimits{20000, 200, 50, 200, 20000, 32 << 20, 8 << 30, 1000, 10000, 8 << 30, 100}
}
func (l *FileLimits) finalize(fail func(string, ...any)) {
	d := DefaultFileLimits()
	for _, p := range []struct {
		name     string
		v        *int
		def, max int
	}{
		{"max_entries", &l.MaxEntries, d.MaxEntries, 20000},
		{"max_page_size", &l.MaxPageSize, d.MaxPageSize, 200},
		{"default_page_size", &l.DefaultPageSize, d.DefaultPageSize, 200},
		{"search_max_results", &l.SearchMaxResults, d.SearchMaxResults, 200},
		{"search_max_examined", &l.SearchMaxExamined, d.SearchMaxExamined, 20000},
		{"trash_max_entries", &l.TrashMaxEntries, d.TrashMaxEntries, 1000},
		{"copy_max_entries", &l.CopyMaxEntries, d.CopyMaxEntries, 10000},
		{"batch_max_items", &l.BatchMaxItems, d.BatchMaxItems, 100},
	} {
		if *p.v == 0 {
			*p.v = p.def
		}
		if *p.v < 1 || *p.v > p.max {
			fail("hub.files.%s must be between 1 and %d", p.name, p.max)
		}
	}
	for _, p := range []struct {
		name     string
		v        *int64
		def, max int64
	}{
		{"preview_max_bytes", &l.PreviewMaxBytes, d.PreviewMaxBytes, 32 << 20},
		{"upload_max_bytes", &l.UploadMaxBytes, d.UploadMaxBytes, MaxFileTransferBytes},
		{"copy_max_bytes", &l.CopyMaxBytes, d.CopyMaxBytes, MaxFileTransferBytes},
	} {
		if *p.v == 0 {
			*p.v = p.def
		}
		if *p.v < 1 || *p.v > p.max {
			fail("hub.files.%s must be between 1 and %d", p.name, p.max)
		}
	}
	if l.DefaultPageSize > l.MaxPageSize {
		l.DefaultPageSize = l.MaxPageSize
	}
	if l.MaxPageSize > l.MaxEntries {
		fail("hub.files.max_page_size must not exceed max_entries")
	}
	if l.SearchMaxResults > l.SearchMaxExamined {
		fail("hub.files.search_max_results must not exceed search_max_examined")
	}
}
