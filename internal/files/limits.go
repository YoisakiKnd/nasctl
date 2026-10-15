package files

import "github.com/YoisakiKnd/nasctl/internal/config"

const MaxTransferBytes = config.MaxFileTransferBytes

func ConfigLimits(l config.FileLimits) Limits {
	return Limits{MaxEntries: l.MaxEntries, MaxPageSize: l.MaxPageSize, DefaultPageSize: l.DefaultPageSize,
		SearchMaxResults: l.SearchMaxResults, SearchMaxExamined: l.SearchMaxExamined,
		PreviewMaxBytes: l.PreviewMaxBytes, UploadMaxBytes: l.UploadMaxBytes, TrashMaxEntries: l.TrashMaxEntries,
		CopyMaxEntries: l.CopyMaxEntries, CopyMaxBytes: l.CopyMaxBytes, BatchMaxItems: l.BatchMaxItems}
}
