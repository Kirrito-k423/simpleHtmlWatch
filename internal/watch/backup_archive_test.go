package watch

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func backupFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "old")
	mustBackupWrite(t, filepath.Join(source, "nested", "config.enc"), []byte("sensitive-backup-fixture"))
	mustBackupWrite(t, filepath.Join(source, "runtime"), []byte("old-binary"))
	if err := os.MkdirAll(filepath.Join(source, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	key, err := archiveKey(base)
	if err != nil {
		t.Fatal(err)
	}
	return source, filepath.Join(base, "saved.shwb"), key
}
func mustBackupWrite(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupEncryptedRoundtripAndDeletion(t *testing.T) {
	source, path, key := backupFixture(t)
	before, err := backupTree(source)
	if err != nil {
		t.Fatal(err)
	}
	r, err := writeBackupArchive(source, path, "legacy", key)
	if err != nil {
		t.Fatal(err)
	}
	if r.Files != 2 {
		t.Fatal(r)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("sensitive-backup-fixture")) {
		t.Fatal("plaintext leaked")
	}
	restored := filepath.Join(filepath.Dir(source), "restore")
	if _, err = verifyBackup(path, key, restored); err != nil {
		t.Fatal(err)
	}
	after, _ := backupTree(restored)
	if !slices.Equal(before, after) {
		t.Fatal("restored tree differs")
	}
	if _, err = verifyBackup(path, key, restored); err == nil {
		t.Fatal("overwrote existing restore")
	}
	if err = removeArchivedSource(source, path, key, filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("verified source not removed")
	}
}

func TestBackupCorruptionAndSourceChangesNeverDelete(t *testing.T) {
	for _, mode := range []string{"corrupt", "wrong-key", "truncated", "changed", "missing-archive"} {
		t.Run(mode, func(t *testing.T) {
			source, path, key := backupFixture(t)
			if _, err := writeBackupArchive(source, path, "legacy", key); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "corrupt":
				b, _ := os.ReadFile(path)
				b[len(b)-1] ^= 1
				mustBackupWrite(t, path, b)
			case "wrong-key":
				key = bytes.Repeat([]byte{9}, 32)
			case "truncated":
				mustBackupWrite(t, path, []byte(backupMagic))
			case "changed":
				mustBackupWrite(t, filepath.Join(source, "runtime"), []byte("changed"))
			case "missing-archive":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := removeArchivedSource(source, path, key, filepath.Dir(source)); err == nil {
				t.Fatal("unsafe deletion succeeded")
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatal("source lost", err)
			}
		})
	}
}

func TestBackupRejectsLinksAndTraversal(t *testing.T) {
	source, path, key := backupFixture(t)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("runtime", filepath.Join(source, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := writeBackupArchive(source, path, "legacy", key); err == nil {
			t.Fatal("archived symbolic link")
		}
		if err := os.Remove(filepath.Join(source, "link")); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"../outside", "/outside", "C:/outside", "a\\outside"} {
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		m := BackupManifest{Version: 1, Entries: []BackupEntry{{Path: name, SHA256: strings.Repeat("0", 64)}}}
		b, _ := json.Marshal(m)
		w, _ := z.Create("manifest.json")
		w.Write(b)
		w, _ = z.Create("payload/" + name)
		w.Write(nil)
		z.Close()
		aead, _ := archiveAEAD(key)
		nonce := make([]byte, aead.NonceSize())
		rand.Read(nonce)
		raw := append([]byte(backupMagic), nonce...)
		raw = aead.Seal(raw, nonce, buf.Bytes(), []byte(backupMagic))
		mustBackupWrite(t, path, raw)
		if _, err := verifyBackup(path, key, filepath.Join(filepath.Dir(source), "bad-restore")); err == nil {
			t.Fatalf("accepted path %q", name)
		}
	}
}

func TestUpgradeSnapshotsAreBoundedAndOldProgramCached(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(t.TempDir(), "program")
	mustBackupWrite(t, old, []byte("version-one"))
	previous := ServiceInfo{Executable: old, BuildID: backupHash([]byte("version-one"))}
	mustBackupWrite(t, filepath.Join(dir, "tasks", "job", "job.json"), []byte("old-state"))
	mustBackupWrite(t, filepath.Join(dir, "tasks", "job", "result.tar.gz"), []byte("unbounded-result-excluded"))
	if err := cacheCurrentProgram(dir, previous); err != nil {
		t.Fatal(err)
	}
	mustBackupWrite(t, old, []byte("new-version-overwrote-old-program"))
	for range 3 {
		if err := prepareUpgradeBackup(dir, previous); err != nil {
			t.Fatal(err)
		}
	}
	r, err := localRollback(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := existingArchiveKey(dir)
	restore := filepath.Join(t.TempDir(), "restored")
	if _, err = verifyBackup(r.ArchivePath, key, restore); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(restore, "runtime", "simpleHtmlWatch"))
	if string(b) != "version-one" {
		t.Fatal("lost old executable")
	}
	if _, err = os.Stat(filepath.Join(restore, "data", "tasks", "job", "result.tar.gz")); !os.IsNotExist(err) {
		t.Fatal("result archived")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "rollback"))
	if len(entries) != 2 {
		t.Fatalf("rollback accumulated: %v", entries)
	}
	status, err := readBackupStatus(dir)
	if err != nil || status.LocalRollback == nil || status.LastArchiveError == "" {
		t.Fatalf("missing offline status: %+v %v", status, err)
	}
	good, _ := os.ReadFile(r.ArchivePath)
	previous.BuildID = strings.Repeat("a", 64)
	if err = prepareUpgradeBackup(dir, previous); err == nil {
		t.Fatal("adopted without old executable")
	}
	after, _ := os.ReadFile(r.ArchivePath)
	if !bytes.Equal(good, after) {
		t.Fatal("failed snapshot replaced valid rollback")
	}
}

func TestBackupStoreLossAndIdentityChangeAreSafe(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(t.TempDir(), "external")
	p, err := backupPolicy(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(store, store+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	if _, err = backupPolicy(dir, store); err != nil {
		t.Fatal("saved offline policy should remain", err)
	}
	if _, err = os.Stat(store); !os.IsNotExist(err) {
		t.Fatal("recreated missing store")
	}
	if checkBackupStore(p) == nil {
		t.Fatal("accepted missing store")
	}
	mustBackupWrite(t, filepath.Join(store, ".simplehtmlwatch-backup-store.json"), []byte(`{"version":1,"id":"different"}`))
	if checkBackupStore(p) == nil {
		t.Fatal("accepted different store")
	}
}

func TestUpgradeArchiveRetentionProtectsTwoAndLegacy(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(t.TempDir(), "external")
	p, err := backupPolicy(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := archiveKey(dir)
	source := filepath.Join(t.TempDir(), "snapshot")
	mustBackupWrite(t, filepath.Join(source, "program"), []byte("program"))
	var paths []string
	for i := 0; i < 5; i++ {
		path := filepath.Join(store, "upgrade-"+string(rune('a'+i))+".shwb")
		r, e := writeBackupArchive(source, path, "upgrade", key)
		if e != nil {
			t.Fatal(e)
		}
		r.CreatedAt = time.Now().Add(-time.Duration(i) * 24 * time.Hour)
		raw, _ := json.Marshal(r)
		mustBackupWrite(t, strings.TrimSuffix(path, ".shwb")+".json", raw)
		paths = append(paths, path)
	}
	legacy := filepath.Join(store, "legacy-preserve.shwb")
	if _, err = writeBackupArchive(source, legacy, "legacy", key); err != nil {
		t.Fatal(err)
	}
	if err = pruneUpgradeArchives(p, key); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		_, e := os.Stat(path)
		if i < 3 && e != nil || i >= 3 && !os.IsNotExist(e) {
			t.Fatalf("wrong count retention %d: %v", i, e)
		}
	}
	p.MaxAgeDays = 1
	if err = pruneUpgradeArchives(p, key); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths[:3] {
		_, e := os.Stat(path)
		if i < 2 && e != nil || i == 2 && !os.IsNotExist(e) {
			t.Fatalf("wrong age retention %d: %v", i, e)
		}
	}
	if _, err = os.Stat(legacy); err != nil {
		t.Fatal("legacy archive removed")
	}
	// Corrupting a newest archive must stop all pruning.
	mustBackupWrite(t, paths[0], []byte("corruption"))
	if err = pruneUpgradeArchives(p, key); err == nil {
		t.Fatal("pruned without two verified backups")
	}
}

func TestBackupMaintenanceLockAndCrashCleanup(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "rollback", ".upgrade-stage-crashed", "data")
	mustBackupWrite(t, stale, []byte("stale"))
	preserve := filepath.Join(dir, "rollback", "last.shwb")
	mustBackupWrite(t, preserve, []byte("keep"))
	unlock, err := lockBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err = os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale stage not cleaned")
	}
	if _, err = os.Stat(preserve); err != nil {
		t.Fatal("rollback deleted")
	}
	if second, e := lockBackups(dir); e == nil {
		second()
		t.Fatal("concurrent backup mutation allowed")
	}
}

func TestUpgradeOversizeStagePreservesLastRollback(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(t.TempDir(), "old-program")
	mustBackupWrite(t, old, []byte("old"))
	previous := ServiceInfo{Executable: old, BuildID: backupHash([]byte("old"))}
	if err := prepareUpgradeBackup(dir, previous); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollback", "last.shwb")
	before, _ := os.ReadFile(path)
	huge := filepath.Join(dir, "tasks", "huge.json")
	mustBackupWrite(t, huge, nil)
	f, err := os.OpenFile(huge, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(backupRawLimit)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = prepareUpgradeBackup(dir, previous); err == nil {
		t.Fatal("oversize snapshot allowed")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed snapshot lost previous rollback")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "rollback"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upgrade-stage-") {
			t.Fatal("failed snapshot left a stage")
		}
	}
}

func TestArchiveCommandRetryRefusesChangedSource(t *testing.T) {
	dir := t.TempDir()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "old")
	mustBackupWrite(t, filepath.Join(source, "data"), []byte("original"))
	c := BackupCommand{ArchiveDir: filepath.Join(t.TempDir(), "external"), Source: source, SourceRoot: root}
	if _, err = RunBackupCommand(dir, c); err != nil {
		t.Fatal(err)
	}
	mustBackupWrite(t, filepath.Join(source, "data"), []byte("changed"))
	c.Remove = true
	if _, err = RunBackupCommand(dir, c); err == nil {
		t.Fatal("removed changed source using old archive")
	}
	if _, err = os.Stat(source); err != nil {
		t.Fatal("changed source lost")
	}
}
