package watch

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const backupArchiveLimit = 128 * 1024 * 1024
const backupRawLimit int64 = 512 * 1024 * 1024
const backupMagic = "SHWBACKUP1\n"

type BackupEntry struct {
	Path       string `json:"path"`
	Directory  bool   `json:"directory,omitempty"`
	Mode       uint32 `json:"mode"`
	ModifiedNS int64  `json:"modifiedNS"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256,omitempty"`
}
type BackupManifest struct {
	Version        int           `json:"version"`
	CreatedAt      time.Time     `json:"createdAt"`
	Kind           string        `json:"kind"`
	OriginalPath   string        `json:"originalPath"`
	RootMode       uint32        `json:"rootMode"`
	RootModifiedNS int64         `json:"rootModifiedNS"`
	Entries        []BackupEntry `json:"entries"`
}
type BackupReceipt struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	CreatedAt    time.Time `json:"createdAt"`
	ArchivePath  string    `json:"archivePath"`
	SHA256       string    `json:"sha256"`
	KeyID        string    `json:"keyId"`
	Files        int       `json:"files"`
	SourceBytes  int64     `json:"sourceBytes"`
	ArchiveBytes int64     `json:"archiveBytes"`
}

func archiveKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, "backup-archive.key")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(key)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return nil, e
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 32 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("备份解密钥匙类型、长度或权限不符")
	}
	return os.ReadFile(path)
}

func existingArchiveKey(dir string) ([]byte, error) {
	if _, err := os.Lstat(filepath.Join(dir, "backup-archive.key")); err != nil {
		return nil, err
	}
	return archiveKey(dir)
}

func backupAtomic(path string, raw, key []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".backup-"+backupHash(key)[:16]+"-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func backupHash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func archiveAEAD(key []byte) (cipher.AEAD, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}

func backupTree(root string) ([]BackupEntry, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("备份来源必须是实际目录")
	}
	entries := []BackupEntry{}
	var total int64
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("备份拒绝链接或特殊文件：%s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		e := BackupEntry{Path: filepath.ToSlash(rel), Directory: info.IsDir(), Mode: uint32(info.Mode().Perm()), ModifiedNS: info.ModTime().UnixNano()}
		if !info.IsDir() {
			e.Size = info.Size()
			total += e.Size
			if total > backupRawLimit {
				return errors.New("备份原始内容超过512 MiB上限")
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			ce := f.Close()
			if err != nil {
				return err
			}
			if ce != nil {
				return ce
			}
			e.SHA256 = hex.EncodeToString(h.Sum(nil))
		}
		entries = append(entries, e)
		if len(entries) > 100000 {
			return errors.New("备份文件数量超过上限")
		}
		return nil
	})
	return entries, err
}

func sealBackup(root, kind string, key []byte) ([]byte, BackupManifest, error) {
	li, e := os.Lstat(root)
	if e != nil {
		return nil, BackupManifest{}, e
	}
	if !li.IsDir() || li.Mode()&os.ModeSymlink != 0 {
		return nil, BackupManifest{}, errors.New("备份来源必须是实际目录")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, BackupManifest{}, err
	}
	root, err = filepath.Abs(canonical)
	if err != nil {
		return nil, BackupManifest{}, err
	}
	manifest := BackupManifest{Version: 1, CreatedAt: time.Now().UTC(), Kind: kind, OriginalPath: root}
	manifest.Entries, err = backupTree(root)
	if err != nil {
		return nil, manifest, err
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, manifest, err
	}
	manifest.RootMode = uint32(rootInfo.Mode().Perm())
	manifest.RootModifiedNS = rootInfo.ModTime().UnixNano()
	var packed bytes.Buffer
	z := zip.NewWriter(&packed)
	for _, e := range manifest.Entries {
		if e.Directory {
			continue
		}
		h := &zip.FileHeader{Name: "payload/" + e.Path, Method: zip.Deflate}
		h.SetMode(os.FileMode(e.Mode))
		h.Modified = time.Unix(0, e.ModifiedNS)
		w, err := z.CreateHeader(h)
		if err != nil {
			return nil, manifest, err
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, manifest, err
		}
		digest := sha256.New()
		n, err := io.Copy(io.MultiWriter(w, digest), io.LimitReader(f, e.Size+1))
		ce := f.Close()
		if err != nil {
			return nil, manifest, err
		}
		if ce != nil {
			return nil, manifest, ce
		}
		if n != e.Size || hex.EncodeToString(digest.Sum(nil)) != e.SHA256 {
			return nil, manifest, errors.New("备份期间源文件变化，保留原件")
		}
		if packed.Len() > backupArchiveLimit {
			return nil, manifest, errors.New("备份压缩内容超过128 MiB上限")
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, manifest, err
	}
	if len(raw) > 16*1024*1024 {
		return nil, manifest, errors.New("备份清单过大")
	}
	w, err := z.Create("manifest.json")
	if err != nil {
		return nil, manifest, err
	}
	if _, err = w.Write(raw); err != nil {
		return nil, manifest, err
	}
	if err = z.Close(); err != nil {
		return nil, manifest, err
	}
	if packed.Len() > backupArchiveLimit-64 {
		return nil, manifest, errors.New("备份压缩内容超过128 MiB上限")
	}
	current, err := backupTree(root)
	if err != nil {
		return nil, manifest, err
	}
	if !slices.Equal(current, manifest.Entries) {
		return nil, manifest, errors.New("备份期间目录变化，保留原件")
	}
	aead, err := archiveAEAD(key)
	if err != nil {
		return nil, manifest, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, manifest, err
	}
	result := append([]byte(backupMagic), nonce...)
	result = aead.Seal(result, nonce, packed.Bytes(), []byte(backupMagic))
	return result, manifest, nil
}

func openBackup(path string, key []byte) (*zip.Reader, BackupManifest, error) {
	var manifest BackupManifest
	f, err := os.Open(path)
	if err != nil {
		return nil, manifest, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, backupArchiveLimit+1))
	if err != nil {
		return nil, manifest, err
	}
	if len(raw) > backupArchiveLimit {
		return nil, manifest, errors.New("备份文件超过128 MiB上限")
	}
	aead, err := archiveAEAD(key)
	if err != nil {
		return nil, manifest, err
	}
	offset := len(backupMagic)
	if len(raw) < offset+aead.NonceSize()+aead.Overhead() || string(raw[:offset]) != backupMagic {
		return nil, manifest, errors.New("备份格式无效")
	}
	plain, err := aead.Open(nil, raw[offset:offset+aead.NonceSize()], raw[offset+aead.NonceSize():], []byte(backupMagic))
	if err != nil {
		return nil, manifest, errors.New("备份解密校验失败")
	}
	z, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		return nil, manifest, err
	}
	mf, err := z.Open("manifest.json")
	if err != nil {
		return nil, manifest, err
	}
	body, err := io.ReadAll(io.LimitReader(mf, 16*1024*1024+1))
	ce := mf.Close()
	if err != nil {
		return nil, manifest, err
	}
	if ce != nil {
		return nil, manifest, ce
	}
	if len(body) > 16*1024*1024 || json.Unmarshal(body, &manifest) != nil || manifest.Version != 1 || len(manifest.Entries) > 100000 || manifest.RootMode&^0777 != 0 {
		return nil, manifest, errors.New("备份清单无效")
	}
	seen := map[string]bool{}
	var size int64
	files := 1
	for _, e := range manifest.Entries {
		if e.Path == "" || e.Path == "." || strings.Contains(e.Path, "\\") || strings.Contains(e.Path, ":") || strings.HasPrefix(e.Path, "/") || !fs.ValidPath(e.Path) || seen[e.Path] || e.Size < 0 || e.Mode&^0777 != 0 {
			return nil, manifest, errors.New("备份路径或字段无效")
		}
		seen[e.Path] = true
		size += e.Size
		if size > backupRawLimit {
			return nil, manifest, errors.New("备份展开大小超限")
		}
		if !e.Directory {
			files++
			if len(e.SHA256) != 64 {
				return nil, manifest, errors.New("备份摘要无效")
			}
		}
	}
	if len(z.File) != files {
		return nil, manifest, errors.New("备份文件数不符")
	}
	return z, manifest, nil
}

func verifyBackup(path string, key []byte, restore string) (BackupManifest, error) {
	z, m, err := openBackup(path, key)
	if err != nil {
		return m, err
	}
	if restore != "" {
		if _, err := os.Lstat(restore); !os.IsNotExist(err) {
			return m, errors.New("恢复目录必须不存在；不会覆盖当前实例")
		}
		if err = os.MkdirAll(restore, 0700); err != nil {
			return m, err
		}
	}
	for _, e := range m.Entries {
		target := filepath.Join(restore, filepath.FromSlash(e.Path))
		if e.Directory {
			if restore != "" {
				if err = os.MkdirAll(target, 0700); err != nil {
					return m, err
				}
			}
			continue
		}
		r, err := z.Open("payload/" + e.Path)
		if err != nil {
			return m, err
		}
		digest := sha256.New()
		var out *os.File
		var w io.Writer = digest
		if restore != "" {
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				r.Close()
				return m, err
			}
			out, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				r.Close()
				return m, err
			}
			w = io.MultiWriter(digest, out)
		}
		n, err := io.Copy(w, io.LimitReader(r, e.Size+1))
		ce := r.Close()
		if out != nil {
			oe := out.Close()
			if err == nil {
				err = oe
			}
		}
		if err != nil {
			return m, err
		}
		if ce != nil {
			return m, ce
		}
		if n != e.Size || hex.EncodeToString(digest.Sum(nil)) != e.SHA256 {
			return m, errors.New("备份文件内容校验失败")
		}
		if restore != "" {
			if err = os.Chmod(target, os.FileMode(e.Mode)); err != nil {
				return m, err
			}
			if err = os.Chtimes(target, time.Unix(0, e.ModifiedNS), time.Unix(0, e.ModifiedNS)); err != nil {
				return m, err
			}
		}
	}
	if restore != "" {
		for i := len(m.Entries) - 1; i >= 0; i-- {
			e := m.Entries[i]
			if !e.Directory {
				continue
			}
			target := filepath.Join(restore, filepath.FromSlash(e.Path))
			if err = os.Chmod(target, os.FileMode(e.Mode)); err != nil {
				return m, err
			}
			if err = os.Chtimes(target, time.Unix(0, e.ModifiedNS), time.Unix(0, e.ModifiedNS)); err != nil {
				return m, err
			}
		}
		actual, err := backupTree(restore)
		if err != nil {
			return m, err
		}
		if !slices.Equal(actual, m.Entries) {
			return m, errors.New("恢复演练的目录或文件与清单不一致")
		}
		if err = os.Chmod(restore, os.FileMode(m.RootMode)); err != nil {
			return m, err
		}
		if err = os.Chtimes(restore, time.Unix(0, m.RootModifiedNS), time.Unix(0, m.RootModifiedNS)); err != nil {
			return m, err
		}
	}
	return m, nil
}

func writeBackupArchive(source, destination, kind string, key []byte) (BackupReceipt, error) {
	var receipt BackupReceipt
	raw, manifest, err := sealBackup(source, kind, key)
	if err != nil {
		return receipt, err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".backup-"+backupHash(key)[:16]+"-")
	if err != nil {
		return receipt, err
	}
	pending := f.Name()
	_ = f.Close()
	defer os.Remove(pending)
	if err = backupAtomic(pending, raw, key); err != nil {
		return receipt, err
	}
	if _, err = verifyBackup(pending, key, ""); err != nil {
		return receipt, err
	}
	if err = os.Rename(pending, destination); err != nil {
		return receipt, err
	}
	return receiptForManifest(destination, raw, manifest, key), nil
}

func receiptForManifest(path string, raw []byte, m BackupManifest, key []byte) BackupReceipt {
	r := BackupReceipt{ID: strings.TrimSuffix(filepath.Base(path), ".shwb"), Kind: m.Kind, CreatedAt: m.CreatedAt, ArchivePath: path, SHA256: backupHash(raw), KeyID: backupHash(key)[:16], ArchiveBytes: int64(len(raw))}
	for _, e := range m.Entries {
		if !e.Directory {
			r.Files++
			r.SourceBytes += e.Size
		}
	}
	return r
}

// The source is removed only after a fresh authenticated read, full content
// verification and a successful restore drill on the local permissioned disk.
func removeArchivedSource(source, path string, key []byte, temporaryParent string) error {
	drill, err := os.MkdirTemp(temporaryParent, ".restore-drill-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(drill)
	m, err := verifyBackup(path, key, filepath.Join(drill, "restored"))
	if err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return err
	}
	if canonical != m.OriginalPath {
		return errors.New("归档来源身份不符，不删除原件")
	}
	current, err := backupTree(source)
	if err != nil {
		return err
	}
	if !slices.Equal(current, m.Entries) {
		return errors.New("源目录已变化，不删除原件")
	}
	staged := source + ".verified-archive-removal"
	if _, err = os.Lstat(staged); !os.IsNotExist(err) {
		return errors.New("归档删除暂存目录已存在，保留原件")
	}
	if err = os.Rename(source, staged); err != nil {
		return err
	}
	current, err = backupTree(staged)
	if err != nil || !slices.Equal(current, m.Entries) {
		_ = os.Rename(staged, source)
		return errors.New("删除前源目录变化，已保留原件")
	}
	// Recheck the external copy after staging, including mount loss or corruption.
	if _, err = verifyBackup(path, key, ""); err != nil {
		_ = os.Rename(staged, source)
		return err
	}
	return os.RemoveAll(staged)
}
