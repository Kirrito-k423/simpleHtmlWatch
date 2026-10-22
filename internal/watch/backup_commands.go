package watch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type BackupCommand struct {
	ArchiveDir string
	Source     string
	SourceRoot string
	Remove     bool
	Verify     string
	Restore    string
	RestoreDir string
	Status     bool
}

// Explicit maintenance commands never start a monitor or SSH client.
func RunBackupCommand(dir string, c BackupCommand) (any, error) {
	unlock, err := lockBackups(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if c.Status {
		return readBackupStatus(dir)
	}
	if c.Verify != "" || c.Restore != "" {
		key, err := existingArchiveKey(dir)
		if err != nil {
			return nil, err
		}
		path, target := c.Verify, ""
		if c.Restore != "" {
			path, target = c.Restore, c.RestoreDir
			if target == "" {
				return nil, errors.New("恢复必须指定新的 -restore-dir")
			}
		}
		m, err := verifyBackup(path, key, target)
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return receiptForManifest(path, raw, m, key), nil
	}
	if c.Source == "" || c.SourceRoot == "" || c.ArchiveDir == "" {
		return nil, errors.New("归档需要明确指定 -archive-backup、-backup-source-root 和 -backup-archive-dir")
	}
	source, err := filepath.Abs(c.Source)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(c.SourceRoot)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	if source != canonical || filepath.Dir(source) != root {
		return nil, errors.New("来源必须是指定根目录的直接实际子目录")
	}
	if source == dir || strings.HasPrefix(dir, source+string(filepath.Separator)) || strings.HasPrefix(source, dir+string(filepath.Separator)) && root != filepath.Join(dir, "service-logs") {
		return nil, errors.New("不能归档当前实例或活动状态目录")
	}
	p, err := backupPolicy(dir, c.ArchiveDir)
	if err != nil {
		return nil, err
	}
	if err = checkBackupStore(p); err != nil {
		return nil, err
	}
	if p.ArchiveDir == source || strings.HasPrefix(p.ArchiveDir, source+string(filepath.Separator)) {
		return nil, errors.New("归档目标不能位于源目录内")
	}
	key, err := archiveKey(dir)
	if err != nil {
		return nil, err
	}
	if err = cleanupArchiveTemps(p, key); err != nil {
		return nil, err
	}
	id := "legacy-" + filepath.Base(source) + "-" + backupHash([]byte(source))[:12]
	path := filepath.Join(p.ArchiveDir, id+".shwb")
	var r BackupReceipt
	if _, err = os.Lstat(path); err == nil {
		m, e := verifyBackup(path, key, "")
		if e != nil {
			return nil, e
		}
		if m.Kind != "legacy" || m.OriginalPath != source {
			return nil, errors.New("同名归档来源不符，不覆盖")
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		r = receiptForManifest(path, raw, m, key)
	} else if os.IsNotExist(err) {
		r, err = writeBackupArchive(source, path, "legacy", key)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	raw, _ := json.MarshalIndent(r, "", "  ")
	if err = backupAtomic(filepath.Join(p.ArchiveDir, id+".json"), raw, key); err != nil {
		return nil, err
	}
	work := filepath.Join(dir, "backup-maintenance")
	if c.Remove {
		err = removeArchivedSource(source, path, key, work)
	} else {
		var drill string
		drill, err = os.MkdirTemp(work, ".restore-drill-")
		if err == nil {
			defer os.RemoveAll(drill)
			_, err = verifyBackup(path, key, filepath.Join(drill, "restored"))
		}
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
