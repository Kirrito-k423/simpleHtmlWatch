package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type BackupPolicy struct {
	Version      int    `json:"version"`
	ArchiveDir   string `json:"archiveDir"`
	StoreID      string `json:"storeId"`
	KeepArchives int    `json:"keepArchives"`
	MaxAgeDays   int    `json:"maxAgeDays"`
}
type BackupStatus struct {
	Policy           BackupPolicy   `json:"policy"`
	LocalRollback    *BackupReceipt `json:"localRollback,omitempty"`
	LastArchiveError string         `json:"lastArchiveError,omitempty"`
	LastCheckedAt    time.Time      `json:"lastCheckedAt"`
	KeyPath          string         `json:"keyPath"`
}

func backupPolicy(dir, requested string) (BackupPolicy, error) {
	p := BackupPolicy{Version: 1, KeepArchives: 3, MaxAgeDays: 30}
	raw, err := os.ReadFile(filepath.Join(dir, "backup-policy.json"))
	if err == nil {
		if json.Unmarshal(raw, &p) != nil || p.Version != 1 || p.KeepArchives < 2 || p.KeepArchives > 20 || p.MaxAgeDays < 1 || p.MaxAgeDays > 365 {
			return p, errors.New("升级备份策略无效，不重置原策略")
		}
	} else if !os.IsNotExist(err) {
		return p, err
	}
	if requested != "" {
		path, err := filepath.Abs(requested)
		if err != nil {
			return p, err
		}
		if p.ArchiveDir == path && p.StoreID != "" {
			return p, nil
		}
		if strings.HasPrefix(path, dir+string(filepath.Separator)) || path == dir {
			return p, errors.New("归档目录不能位于实例目录内")
		}
		// Mkdir, not MkdirAll: an absent external mount must not be recreated on Mac.
		if err = os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return p, err
		}
		marker := filepath.Join(path, ".simplehtmlwatch-backup-store.json")
		var id struct {
			Version int    `json:"version"`
			ID      string `json:"id"`
		}
		raw, err := os.ReadFile(marker)
		if os.IsNotExist(err) {
			id.Version, id.ID = 1, randomToken()
			raw, _ = json.Marshal(id)
			err = writeAtomic(marker, raw)
		} else if err == nil {
			err = json.Unmarshal(raw, &id)
		}
		if err != nil || id.Version != 1 || id.ID == "" {
			return p, errors.New("外置归档仓库标识无效")
		}
		p.ArchiveDir, p.StoreID = path, id.ID
		raw, _ = json.MarshalIndent(p, "", "  ")
		if err = writeAtomic(filepath.Join(dir, "backup-policy.json"), raw); err != nil {
			return p, err
		}
	}
	return p, nil
}

func checkBackupStore(p BackupPolicy) error {
	if p.ArchiveDir == "" {
		return errors.New("未配置外置升级备份目录；本机保留一份回滚快照")
	}
	var marker struct {
		Version int    `json:"version"`
		ID      string `json:"id"`
	}
	raw, err := os.ReadFile(filepath.Join(p.ArchiveDir, ".simplehtmlwatch-backup-store.json"))
	if err != nil {
		return fmt.Errorf("外置归档不可用，保留本机回滚：%w", err)
	}
	if json.Unmarshal(raw, &marker) != nil || marker.Version != 1 || marker.ID != p.StoreID {
		return errors.New("外置归档仓库身份改变，保留本机回滚")
	}
	return nil
}

func copyBackupFile(from, to string) error {
	li, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !li.Mode().IsRegular() {
		return errors.New("备份拒绝链接或特殊文件")
	}
	f, err := os.Open(from)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > backupRawLimit {
		return errors.New("升级备份文件类型或大小无效")
	}
	if err = os.MkdirAll(filepath.Dir(to), 0700); err != nil {
		return err
	}
	w, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(w, io.LimitReader(f, info.Size()+1))
	err = copyErr
	if err == nil && n != info.Size() {
		err = errors.New("备份期间文件大小改变")
	}
	if err == nil {
		err = w.Sync()
	}
	ce := w.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Chtimes(to, info.ModTime(), info.ModTime())
}

// Called only after the instance OS lock is held and a changed build has been
// explicitly adopted. Snapshot the old format before the new program loads it.
func prepareUpgradeBackup(dir string, previous ServiceInfo) error {
	unlock, err := lockBackups(dir)
	if err != nil {
		return err
	}
	defer unlock()
	rollback := filepath.Join(dir, "rollback")
	if err := os.MkdirAll(rollback, 0700); err != nil {
		return err
	}
	key, err := archiveKey(dir)
	if err != nil {
		return err
	}
	// Preserve a previously deferred archive before replacing the local slot.
	if p, e := backupPolicy(dir, ""); e == nil && checkBackupStore(p) == nil {
		if _, e = os.Stat(filepath.Join(rollback, "last.shwb")); e == nil {
			if e = archiveLatestBackupLocked(dir); e != nil {
				return e
			}
		}
	}
	stage, err := os.MkdirTemp(rollback, ".upgrade-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	var stagedBytes int64
	copyCore := func(from, to string) error {
		info, e := os.Lstat(from)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || stagedBytes+info.Size() > backupRawLimit {
			return errors.New("升级核心快照超过512 MiB或包含特殊文件")
		}
		stagedBytes += info.Size()
		return copyBackupFile(from, to)
	}
	old := previous.Executable
	oldBytes, err := readBackupProgram(old)
	if err != nil || backupHash(oldBytes) != previous.BuildID {
		old = filepath.Join(rollback, "current-program")
		oldBytes, err = readBackupProgram(old)
		if err != nil || backupHash(oldBytes) != previous.BuildID {
			return errors.New("旧构建已被覆盖且无匹配的回滚缓存；请从保留的旧发布目录升级")
		}
	}
	if err = copyCore(old, filepath.Join(stage, "runtime", "simpleHtmlWatch")); err != nil {
		return err
	}
	if err = os.Chmod(filepath.Join(stage, "runtime", "simpleHtmlWatch"), 0700); err != nil {
		return err
	}
	for _, name := range []string{"config.enc", "vault.key", "known_hosts.json", "service.json", "backup-policy.json"} {
		from := filepath.Join(dir, name)
		if _, err = os.Stat(from); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err = copyCore(from, filepath.Join(stage, "data", name)); err != nil {
			return err
		}
	}
	for _, folder := range []string{"tasks", "utilization", "occupancy", "exports"} {
		base := filepath.Join(dir, folder)
		if _, err = os.Stat(base); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		err = filepath.WalkDir(base, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".json") && !strings.HasSuffix(d.Name(), ".json.gz") {
				return nil
			}
			rel, e := filepath.Rel(dir, path)
			if e != nil {
				return e
			}
			return copyCore(path, filepath.Join(stage, "data", rel))
		})
		if err != nil {
			return err
		}
	}
	last := filepath.Join(rollback, "last.shwb")
	// The authenticated archive is the commit record. No second metadata file
	// needs to be committed with it; a crash always leaves one complete snapshot.
	if _, err = writeBackupArchive(stage, last, "upgrade", key); err != nil {
		return err
	}
	// External media failure never removes the verified local fallback.
	_ = archiveLatestBackupLocked(dir)
	return nil
}

func cacheCurrentProgram(dir string, info ServiceInfo) error {
	rollback := filepath.Join(dir, "rollback")
	if err := os.MkdirAll(rollback, 0700); err != nil {
		return err
	}
	cache := filepath.Join(rollback, "current-program")
	if b, e := readBackupProgram(cache); e == nil && backupHash(b) == info.BuildID {
		return nil
	}
	b, err := readBackupProgram(info.Executable)
	if err != nil {
		return err
	}
	if len(b) > 64*1024*1024 || backupHash(b) != info.BuildID {
		return errors.New("当前构建无法安全缓存")
	}
	return writeAtomic(cache, b)
}

func readBackupProgram(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64*1024*1024 {
		return nil, errors.New("备份程序类型错误或超过64 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64*1024*1024+1))
	if len(b) > 64*1024*1024 {
		return nil, errors.New("备份程序超过64 MiB")
	}
	return b, err
}

func readBackupStatus(dir string) (BackupStatus, error) {
	p, err := backupPolicy(dir, "")
	if err != nil {
		return BackupStatus{}, err
	}
	s := BackupStatus{Policy: p, KeyPath: filepath.Join(dir, "backup-archive.key")}
	raw, err := os.ReadFile(filepath.Join(dir, "backup-status.json"))
	if err == nil {
		if json.Unmarshal(raw, &s) != nil {
			return s, errors.New("备份状态格式无效")
		}
		s.Policy = p
	} else if !os.IsNotExist(err) {
		return s, err
	}
	s.LocalRollback = nil
	if r, e := localRollback(dir); e == nil {
		s.LocalRollback = &r
	} else if !os.IsNotExist(e) {
		return s, e
	}
	return s, nil
}

func archiveLatestBackup(dir string) error {
	unlock, err := lockBackups(dir)
	if err != nil {
		return err
	}
	defer unlock()
	return archiveLatestBackupLocked(dir)
}
func archiveLatestBackupLocked(dir string) error {
	status, err := readBackupStatus(dir)
	if err != nil {
		return err
	}
	status.LastCheckedAt = time.Now().UTC()
	status.LastArchiveError = ""
	defer func() {
		raw, _ := json.MarshalIndent(status, "", "  ")
		_ = writeAtomic(filepath.Join(dir, "backup-status.json"), raw)
	}()
	if status.LocalRollback == nil {
		return nil
	}
	receipt := *status.LocalRollback
	err = publishArchivedBackup(dir, status.Policy, receipt)
	if err != nil {
		status.LastArchiveError = err.Error()
		return err
	}
	return nil
}

func publishArchivedBackup(dir string, p BackupPolicy, receipt BackupReceipt) error {
	if err := checkBackupStore(p); err != nil {
		return err
	}
	key, err := archiveKey(dir)
	if err != nil {
		return err
	}
	if err = cleanupArchiveTemps(p, key); err != nil {
		return err
	}
	if _, err = verifyBackup(receipt.ArchivePath, key, ""); err != nil {
		return err
	}
	destination := filepath.Join(p.ArchiveDir, receipt.ID+".shwb")
	existing, e := os.ReadFile(destination)
	if e == nil {
		if backupHash(existing) != receipt.SHA256 {
			return errors.New("外置同名归档内容不符，不覆盖")
		}
	} else if os.IsNotExist(e) {
		b, e := os.ReadFile(receipt.ArchivePath)
		if e != nil {
			return e
		}
		if backupHash(b) != receipt.SHA256 {
			return errors.New("本机回滚摘要不符")
		}
		if e = backupAtomic(destination, b, key); e != nil {
			return e
		}
	} else {
		return e
	}
	if _, err = verifyBackup(destination, key, ""); err != nil {
		return err
	}
	receipt.ArchivePath = destination
	raw, _ := json.MarshalIndent(receipt, "", "  ")
	if err = backupAtomic(filepath.Join(p.ArchiveDir, receipt.ID+".json"), raw, key); err != nil {
		return err
	}
	return pruneUpgradeArchives(p, key)
}

func cleanupArchiveTemps(p BackupPolicy, key []byte) error {
	if err := checkBackupStore(p); err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(p.ArchiveDir, ".backup-"+backupHash(key)[:16]+"-*"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("归档临时文件类型异常，保留")
		}
		if e = os.Remove(path); e != nil {
			return e
		}
	}
	return nil
}

func pruneUpgradeArchives(p BackupPolicy, key []byte) error {
	if err := checkBackupStore(p); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(p.ArchiveDir, "upgrade-*.json"))
	if err != nil {
		return err
	}
	var receipts []BackupReceipt
	for _, file := range files {
		var r BackupReceipt
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if json.Unmarshal(raw, &r) != nil || r.Kind != "upgrade" || r.KeyID != backupHash(key)[:16] {
			continue
		}
		if r.ID != strings.TrimSuffix(filepath.Base(file), ".json") || r.ArchivePath != filepath.Join(p.ArchiveDir, r.ID+".shwb") {
			return errors.New("外置备份收据路径不符，不清理")
		}
		receipts = append(receipts, r)
	}
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].CreatedAt.After(receipts[j].CreatedAt) })
	// Two independently verified newest archives are always protected, even old.
	for i := 0; i < min(2, len(receipts)); i++ {
		b, err := os.ReadFile(receipts[i].ArchivePath)
		if err != nil {
			return err
		}
		if backupHash(b) != receipts[i].SHA256 {
			return errors.New("最新回滚归档摘要不符，不清理旧归档")
		}
		if _, err = verifyBackup(receipts[i].ArchivePath, key, ""); err != nil {
			return err
		}
	}
	for i, r := range receipts {
		if i < 2 || i < p.KeepArchives && time.Since(r.CreatedAt) < time.Duration(p.MaxAgeDays)*24*time.Hour {
			continue
		}
		if err = os.Remove(r.ArchivePath); err != nil {
			return err
		}
		if err = os.Remove(filepath.Join(p.ArchiveDir, r.ID+".json")); err != nil {
			return err
		}
	}
	return nil
}

func StartBackupMaintenance(dir string, info ServiceInfo) (func(), error) {
	unlock, err := lockBackups(dir)
	if err != nil {
		return nil, err
	}
	err = cacheCurrentProgram(dir, info)
	unlock()
	if err != nil {
		return nil, err
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = archiveLatestBackup(dir)
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = archiveLatestBackup(dir)
			}
		}
	}()
	return func() { close(stop); <-done }, nil
}

func localRollback(dir string) (BackupReceipt, error) {
	path := filepath.Join(dir, "rollback", "last.shwb")
	if _, err := os.Stat(path); err != nil {
		return BackupReceipt{}, err
	}
	key, err := existingArchiveKey(dir)
	if err != nil {
		return BackupReceipt{}, err
	}
	m, err := verifyBackup(path, key, "")
	if err != nil {
		return BackupReceipt{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return BackupReceipt{}, err
	}
	r := receiptForManifest(path, raw, m, key)
	r.ID = "upgrade-" + m.CreatedAt.UTC().Format("20060102T150405.000000000") + "-" + r.KeyID
	return r, nil
}

func lockBackups(dir string) (func(), error) {
	work := filepath.Join(dir, "backup-maintenance")
	unlock, err := LockDirectory(work)
	if err != nil {
		return nil, err
	}
	// Only our private work areas are swept, while holding their OS lock.
	for _, base := range []string{work, filepath.Join(dir, "rollback")} {
		entries, e := os.ReadDir(base)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			unlock()
			return nil, e
		}
		for _, entry := range entries {
			n := entry.Name()
			if strings.HasPrefix(n, ".restore-drill-") || strings.HasPrefix(n, ".upgrade-stage-") || strings.HasPrefix(n, ".watch-") || strings.HasPrefix(n, ".backup-") {
				if e = os.RemoveAll(filepath.Join(base, n)); e != nil {
					unlock()
					return nil, e
				}
			}
		}
	}
	return unlock, nil
}
