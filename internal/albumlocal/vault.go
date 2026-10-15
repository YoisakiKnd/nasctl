package albumlocal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/cachepath"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/photovault"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

func (c *Control) MoveToVault(ctx context.Context, vault *photovault.Store, token, owner string, id int64, details album.Details) (string, error) {
	c.mu.Lock()
	if c.busy {
		c.mu.Unlock()
		return "", errors.New("请等待当前相册任务结束")
	}
	cfg, e := c.effective(c.directories)
	if e != nil {
		c.mu.Unlock()
		return "", e
	}
	c.busy = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.busy = false; c.mu.Unlock() }()
	vaultID, record, exists, e := vault.Find(token, owner, id)
	if e != nil {
		return "", e
	}
	st, e := store.Open(cfg.Paths.IndexFile, store.Options{})
	if e != nil {
		return "", errors.New("无法打开相册索引")
	}
	defer st.Close()
	if exists {
		if e = vault.Verify(ctx, token, owner, vaultID); e != nil {
			return vaultID, e
		}
	}
	var thumbnail []byte
	if !exists {
		media, e := st.GetMedia(ctx, id)
		if e != nil {
			return "", e
		}
		if media.Kind != "image" || media.Missing || media.Size > photovault.MaxPhotoBytes {
			return "", errors.New("保险箱目前支持不超过 64 MiB 的照片")
		}
		col, ok := cfg.Collection(media.CollectionCode)
		if !ok || !col.Enabled {
			return "", errors.New("照片文件夹已停用")
		}
		joined, e := fsutil.SafeRelJoin(col.Root, media.RelPath)
		if e != nil {
			return "", e
		}
		// The same file indexed through another configured collection would retain
		// a second public thumbnail. Refuse this ambiguous move before copying.
		sourceReal, resolveErr := filepath.EvalSymlinks(joined)
		if resolveErr != nil {
			return "", errors.New("照片原件暂不可用")
		}
		for _, other := range cfg.Collections {
			if other.Code == media.CollectionCode {
				continue
			}
			otherRoot, resolveErr := filepath.EvalSymlinks(other.Root)
			if resolveErr == nil && fsutil.IsWithin(otherRoot, sourceReal) {
				return "", errors.New("此照片被多个相册目录引用，请先移除重叠的目录配置")
			}
		}
		for _, root := range cfg.Hub.Storage {
			if fsutil.IsWithin(root.Path, col.Root) && root.Writable {
				relative, e := filepath.Rel(root.Path, joined)
				if e != nil {
					return "", e
				}
				record.SourceRoot = root.ID
				record.SourcePath = filepath.ToSlash(relative)
				break
			}
		}
		if record.SourceRoot == "" {
			return "", errors.New("照片目录需要属于允许写入的存储位置，才能移入保险箱")
		}
		record.Details = details
		record.MtimeNS = media.MtimeNS
		record.CacheKey = cachepath.Key(media.CollectionCode, media.RelPath)
		// Read only an existing cache file. Missing thumbnails get an encrypted placeholder.
		thumb, e := cachepath.Path(cfg.Paths.CacheDir, record.CacheKey, cfg.Decode.ThumbMaxEdge, cfg.Cache.ShardDepth)
		if e != nil {
			return "", e
		}
		f, e := os.OpenFile(thumb, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e == nil {
			info, se := f.Stat()
			if se != nil || !info.Mode().IsRegular() || info.Size() > album.MaxThumbnailBytes {
				f.Close()
				return "", errors.New("照片缩略图无效")
			}
			thumbnail, e = io.ReadAll(io.LimitReader(f, album.MaxThumbnailBytes+1))
			f.Close()
			if e != nil {
				return "", e
			}
			record.ThumbMIME = "image/jpeg"
		} else if !os.IsNotExist(e) {
			return "", errors.New("无法读取照片缩略图")
		}
		if len(thumbnail) == 0 {
			thumbnail = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 240 240"><rect width="240" height="240" fill="#e8edea"/><path d="M65 70h110v100H65z M65 155l35-35 25 25 20-20 30 30" fill="none" stroke="#6d8c83" stroke-width="6"/></svg>`)
			record.ThumbMIME = "image/svg+xml"
		}
	}
	e = c.files.ConsumeSealed(ctx, record.SourceRoot, record.SourcePath, record.Details.Item.Bytes, record.MtimeNS, func(f *os.File) (string, error) {
		var digest string
		vaultID, digest, e = vault.Put(ctx, token, owner, record, f, thumbnail)
		return digest, e
	}, record.Digest)
	if e != nil {
		return vaultID, e
	}
	// Finish privacy cleanup even if the requester disconnects after unlinking.
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	current, lookupErr := st.GetMedia(cleanup, id)
	if lookupErr == nil && cachepath.Key(current.CollectionCode, current.RelPath) != record.CacheKey {
		return vaultID, errors.New("相册索引已经变更，请在本机检查未完成的移入操作")
	}
	if e = st.SealMedia(cleanup, id); e != nil {
		return vaultID, errors.New("加密副本已保存，普通相册清理待重试")
	}
	root, e := os.OpenRoot(cfg.Paths.CacheDir)
	if e != nil {
		return vaultID, e
	}
	defer root.Close()
	shard := cachepath.ShardDir(record.CacheKey, cfg.Cache.ShardDepth)
	dir, e := root.Open(shard)
	if e == nil {
		defer dir.Close()
		entries, e := dir.ReadDir(4097)
		if e != nil && e != io.EOF {
			return vaultID, e
		}
		if len(entries) > 4096 {
			return vaultID, errors.New("缩略图目录超过清理限额")
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), record.CacheKey+"_") && cachepath.IsCacheArtifact(entry.Name()) {
				if !entry.Type().IsRegular() {
					return vaultID, errors.New("缩略图缓存包含非普通文件，清理待重试")
				}
				if e = root.Remove(filepath.Join(shard, entry.Name())); e != nil {
					return vaultID, e
				}
			}
		}
	} else if !os.IsNotExist(e) {
		return vaultID, e
	}
	if e = vault.Complete(token, owner, vaultID); e != nil {
		return vaultID, e
	}
	return vaultID, nil
}
