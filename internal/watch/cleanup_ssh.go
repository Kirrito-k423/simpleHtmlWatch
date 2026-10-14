package watch

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
)

//go:embed cleanup_remote.py
var cleanupPython string

func (r *sshTaskRemote) Cleanup(ctx context.Context, h Machine, p Profile, q cleanupRequest) (CleanupSnapshot, string, error) {
	// Identity hashes and log fingerprints are enough for the guard; keep full evidence locally.
	compact := func(s *CleanupSnapshot) *CleanupSnapshot {
		if s == nil {
			return nil
		}
		c := *s
		c.NPU = ""
		c.Command = ""
		c.Logs = append([]CleanupLog(nil), s.Logs...)
		for i := range c.Logs {
			c.Logs[i].Tail = ""
		}
		return &c
	}
	q.Baseline, q.Last = compact(q.Baseline), compact(q.Last)
	raw, err := json.Marshal(q)
	if err != nil {
		return CleanupSnapshot{}, "", err
	}
	shell := "python3 - " + shellQuote(string(raw)) + " <<'SHW_CLEANUP_PY'\n" + cleanupPython + "\nSHW_CLEANUP_PY\n"
	result, err := r.command(ctx, TaskJob{Host: h.Host, Port: h.Port}, p, shell)
	if result.Truncated {
		return CleanupSnapshot{}, "", errors.New("CLEANUP_REPLY_TRUNCATED")
	}
	if err != nil {
		return CleanupSnapshot{}, result.Stdout, err
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err = json.Unmarshal([]byte(result.Stdout), &envelope); err != nil {
		return CleanupSnapshot{}, "", err
	}
	if envelope.Error != "" {
		return CleanupSnapshot{}, result.Stdout, errors.New(envelope.Error)
	}
	var snapshot CleanupSnapshot
	if q.Mode == "inspect" {
		if err = json.Unmarshal([]byte(result.Stdout), &snapshot); err != nil {
			return snapshot, "", err
		}
	}
	return snapshot, result.Stdout, nil
}
