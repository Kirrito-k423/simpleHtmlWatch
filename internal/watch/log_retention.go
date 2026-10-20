package watch

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const logFileLimit = 64 * 1024
const logTailBytes = 32 * 1024

// BoundedLogs only trims explicitly selected diagnostic logs. It never renames
// files: launchd/SSH writers retain the same open inode and continue appending.
type BoundedLogs struct {
	dir, root string
	paths     []string
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
}

func NewBoundedLogs(dir string, paths []string) (*BoundedLogs, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	b := &BoundedLogs{dir: dir, root: root, stop: make(chan struct{}), done: make(chan struct{})}
	seen := map[string]bool{}
	for _, path := range paths {
		path = filepath.Clean(path)
		if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) || filepath.Ext(path) != ".log" {
			return nil, fmt.Errorf("bounded-log must name a diagnostic .log file relative to the data directory: %s", path)
		}
		if !seen[path] {
			seen[path] = true
			b.paths = append(b.paths, path)
		}
	}
	if len(b.paths) > 8 {
		return nil, fmt.Errorf("at most 8 bounded diagnostic logs are allowed")
	}
	// Validate all targets before changing any file.
	for _, path := range b.paths {
		if _, err := b.logInfo(path); err != nil {
			return nil, err
		}
	}
	if err := b.trim(); err != nil {
		return nil, err
	}
	go func() {
		defer close(b.done)
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-tick.C:
				if err := b.trim(); err != nil {
					fmt.Fprintln(os.Stderr, "simpleHtmlWatch bounded logs:", err)
				}
			}
		}
	}()
	return b, nil
}

func (b *BoundedLogs) Close() {
	b.once.Do(func() { close(b.stop) })
	<-b.done
}

func (b *BoundedLogs) logInfo(relative string) (os.FileInfo, error) {
	path := filepath.Join(b.dir, relative)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || resolved != filepath.Join(b.root, relative) {
		return nil, fmt.Errorf("bounded log must be a regular file without symlink components: %s", relative)
	}
	return info, nil
}

func (b *BoundedLogs) trim() error {
	for _, relative := range b.paths {
		info, err := b.logInfo(relative)
		if err != nil {
			return err
		}
		if info == nil || info.Size() <= logFileLimit {
			continue
		}
		if err := b.trimFile(relative, info); err != nil {
			return err
		}
	}
	return nil
}

func (b *BoundedLogs) trimFile(relative string, before os.FileInfo) error {
	f, err := os.OpenFile(filepath.Join(b.dir, relative), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, current) || !current.Mode().IsRegular() {
		return fmt.Errorf("bounded log changed during inspection: %s", relative)
	}
	if current.Size() <= logFileLimit {
		return nil
	}
	tail := make([]byte, logTailBytes)
	n, err := f.ReadAt(tail, current.Size()-logTailBytes)
	if err != nil && err != io.EOF {
		return err
	}
	tail = tail[:n]
	if newline := bytes.IndexByte(tail, '\n'); newline >= 0 {
		tail = tail[newline+1:]
	}
	// Diagnostic writers may append concurrently. A few lines at the cutover can
	// be lost; task records and experiment output must never be selected here.
	if err = f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(tail, 0)
	return err
}
