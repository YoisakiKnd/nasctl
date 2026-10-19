package files

import (
	"archive/tar"
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

var (
	// ErrNotZip refuses something that is not a readable ZIP archive.
	ErrNotZip = errors.New("files: not a readable ZIP archive")
	// ErrZipUnsupported refuses an encrypted entry or one compressed with a
	// method other than store or deflate.
	ErrZipUnsupported = errors.New("files: the ZIP archive is encrypted or uses an unsupported compression method")
)

// zipDirectoryMax bounds the central directory read into memory: about
// 10000 entries with long names. archive/zip holds every entry it lists.
const zipDirectoryMax = 8 << 20

// ExtractResult reports an extracted archive.
type ExtractResult struct {
	CopyResult
	// Links counts symbolic links and other special entries left out.
	Links int `json:"skipped_links"`
}

// ExtractZip unpacks the ZIP file rel into a new folder destination in the
// same root. It reuses the tar import: entries are checked for unsafe names,
// counted against limit and maxBytes, written in staging and published only
// when the whole archive has been read and the ZIP file has not changed.
// Memory stays bounded by the central directory and one deflate stream.
func (m *Manager) ExtractZip(ctx context.Context, rootID, rel, destination string, limit int, maxBytes int64, progress func(done, total int64)) (ExtractResult, error) {
	result := ExtractResult{CopyResult: CopyResult{DestinationRoot: rootID, Path: destination}}
	if limit < 1 || limit > m.limits.CopyMaxEntries || maxBytes < 1 || maxBytes > m.limits.CopyMaxBytes {
		return result, ErrTreeLimit
	}
	f, info, err := m.Open(rootID, rel)
	if err != nil {
		return result, err
	}
	defer f.Close()
	if err = zipDirectoryFits(f, info.Size(), limit); err != nil {
		return result, err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		if errors.Is(err, zip.ErrInsecurePath) {
			return result, ErrBadPath
		}
		return result, ErrNotZip
	}
	type file struct {
		name string
		f    *zip.File
	}
	var entries []file
	folders := map[string]time.Time{}
	var total int64
	for _, zf := range zr.File {
		name := zipName(zf)
		folder := strings.HasSuffix(name, "/")
		name = strings.TrimSuffix(name, "/")
		// macOS adds resource forks under __MACOSX; they are not content.
		if name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		mode := zf.Mode()
		switch {
		case folder || mode.IsDir():
			folders[name] = zf.Modified
			continue
		case !mode.IsRegular():
			result.Links++
			continue
		case zf.Flags&0x1 != 0 || zf.Method != zip.Store && zf.Method != zip.Deflate:
			return result, ErrZipUnsupported
		}
		entries = append(entries, file{name, zf})
		total += int64(zf.UncompressedSize64)
		if len(entries) > limit || zf.UncompressedSize64 > uint64(maxBytes) || total > maxBytes {
			return result, ErrTreeLimit
		}
		// Folders an archive only implies are made too.
		for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if _, ok := folders[dir]; !ok {
				folders[dir] = time.Time{}
			}
		}
	}
	names := make([]string, 0, len(folders))
	for name := range folders {
		names = append(names, name)
	}
	// A parent sorts before its children, so each folder exists when needed.
	sort.Strings(names)

	reader, writer := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		err := func() error {
			tw := tar.NewWriter(writer)
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "data/"}); err != nil {
				return err
			}
			for _, name := range names {
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "data/" + name + "/", ModTime: folders[name]}); err != nil {
					return err
				}
			}
			for _, e := range entries {
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "data/" + e.name, Size: int64(e.f.UncompressedSize64), ModTime: e.f.Modified}); err != nil {
					return err
				}
				rc, err := e.f.Open()
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, rc)
				rc.Close()
				if err != nil {
					return err
				}
			}
			return tw.Close()
		}()
		produced <- err
		writer.CloseWithError(err)
	}()
	verify := func() error {
		// The archive must still be the one that was read.
		now, err := f.Stat()
		if err != nil || now.Size() != info.Size() || !now.ModTime().Equal(info.ModTime()) {
			return ErrSourceChanged
		}
		return nil
	}
	copied, err := m.importTar(ctx, rootID, destination, reader, limit, maxBytes, verify, func(_, bytes int64, _ string) {
		if progress != nil {
			progress(bytes, total)
		}
	}, true)
	reader.CloseWithError(errors.New("files: extraction stopped"))
	result.CopyResult = copied
	// A damaged archive surfaces in the import as a broken stream; report
	// what the ZIP reader found instead.
	if zipErr := <-produced; err != nil && zipErr != nil {
		switch {
		case errors.Is(zipErr, zip.ErrAlgorithm):
			err = ErrZipUnsupported
		case errors.Is(zipErr, zip.ErrFormat), errors.Is(zipErr, zip.ErrChecksum), errors.Is(zipErr, io.ErrUnexpectedEOF):
			err = ErrNotZip
		}
	}
	return result, err
}

// zipName is the entry's name as UTF-8. Archives made on Chinese Windows
// store GBK names without saying so; an Info-ZIP Unicode Path field, when
// present and matching, carries the real name. Backslash separators from
// old Windows tools become slashes.
func zipName(f *zip.File) string {
	name := f.Name
	for extra := f.Extra; len(extra) >= 4; {
		id, size := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		if len(extra) < 4+size {
			break
		}
		field := extra[4 : 4+size]
		if id == 0x7075 && size > 5 && field[0] == 1 && binary.LittleEndian.Uint32(field[1:]) == crc32.ChecksumIEEE([]byte(f.Name)) && utf8.Valid(field[5:]) {
			name = string(field[5:])
			break
		}
		extra = extra[4+size:]
	}
	if !utf8.ValidString(name) {
		if decoded, err := simplifiedchinese.GB18030.NewDecoder().String(name); err == nil {
			name = decoded
		}
	}
	return strings.ReplaceAll(name, "\\", "/")
}

// zipDirectoryFits reads the end-of-central-directory record before
// archive/zip does, so an archive listing more entries than the budget, or
// a huge directory, is refused before it is held in memory.
func zipDirectoryFits(r io.ReaderAt, size int64, limit int) error {
	const endLen, locatorLen, end64Len = 22, 20, 56
	tail := min(size, endLen+65535)
	if tail < endLen {
		return ErrNotZip
	}
	b := make([]byte, tail)
	if _, err := r.ReadAt(b, size-tail); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	at := -1
	for i := len(b) - endLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(b[i:]) == 0x06054b50 {
			at = i
			break
		}
	}
	if at < 0 {
		return ErrNotZip
	}
	records, directory := uint64(binary.LittleEndian.Uint16(b[at+10:])), uint64(binary.LittleEndian.Uint32(b[at+12:]))
	if records == 0xffff || directory == 0xffffffff {
		// ZIP64: the real counts are in a record the locator points to.
		offset := size - tail + int64(at) - locatorLen
		locator := make([]byte, locatorLen)
		if offset < 0 {
			return ErrNotZip
		}
		if _, err := r.ReadAt(locator, offset); err != nil || binary.LittleEndian.Uint32(locator) != 0x07064b50 {
			return ErrNotZip
		}
		end := make([]byte, end64Len)
		at64 := binary.LittleEndian.Uint64(locator[8:])
		if at64 > uint64(size) {
			return ErrNotZip
		}
		if _, err := r.ReadAt(end, int64(at64)); err != nil || binary.LittleEndian.Uint32(end) != 0x06064b50 {
			return ErrNotZip
		}
		records, directory = binary.LittleEndian.Uint64(end[32:]), binary.LittleEndian.Uint64(end[40:])
	}
	// Skipped __MACOSX entries can double a Mac archive's count.
	if records > 2*uint64(limit) || directory > zipDirectoryMax {
		return ErrTreeLimit
	}
	return nil
}
