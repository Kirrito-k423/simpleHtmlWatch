package watch

import (
	"context"
	"os"
	"path/filepath"
)

func (r *sshTaskRemote) Collect(ctx context.Context, job TaskJob, profile Profile, destination string) error {
	// Retained for the local transport fixture; production downloads use Stream.
	f, err := os.CreateTemp(filepath.Dir(destination), ".archive-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if err := r.Stream(ctx, job, profile, f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), destination)
}
