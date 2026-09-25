package watch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const maxTaskArchive = 5 * 1024 * 1024

type sshTaskRemote struct{ trust *TrustStore }

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func taskDir(id string) string { return `"$HOME/.simplehtmlwatch/tasks/` + id + `"` }

func (r *sshTaskRemote) command(ctx context.Context, job TaskJob, profile Profile, shell string) (Result, error) {
	addr := net.JoinHostPort(job.Host, strconv.Itoa(job.Port))
	client, conn, err := dial(ctx, addr, profile, r.trust.Callback(addr, false))
	if err != nil {
		return Result{}, err
	}
	defer client.Close()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer session.Close()
	var stdout, stderr boundedOutput
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Run("bash -o pipefail -c " + shellQuote(shell))
	result := Result{Stdout: stdout.b.String(), Stderr: stderr.b.String(), Truncated: stdout.truncated || stderr.truncated}
	if err == nil {
		code := 0
		result.ExitCode = &code
		return result, nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitStatus()
		result.ExitCode = &code
	}
	return result, fmt.Errorf("远端任务控制失败：%w：%s", err, result.Stderr)
}

func (r *sshTaskRemote) Launch(ctx context.Context, job TaskJob, profile Profile) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(job.Shell))
	runner := "#!/usr/bin/env bash\n" +
		"set +e\n" +
		"dir=" + taskDir(job.ID) + "\n" +
		"export SHW_RESULTS_DIR=\"$dir/results\"\n" +
		"cd \"$SHW_RESULTS_DIR\" || exit 111\n" +
		"bash -o pipefail -lc \"$(cat \"$dir/command.sh\")\" >\"$dir/stdout.log\" 2>\"$dir/stderr.log\"\n" +
		"code=$?\n" +
		"printf '%s\\n' \"$code\" >\"$dir/exit.code.tmp\"\n" +
		"mv \"$dir/exit.code.tmp\" \"$dir/exit.code\"\n"
	script := "set -e\numask 077\nbase=\"$HOME/.simplehtmlwatch/tasks\"\n" +
		"mkdir -p \"$base\"\ndir=" + taskDir(job.ID) + "\n" +
		"mkdir \"$dir\"\nmkdir \"$dir/results\"\n" +
		"printf '%s' " + shellQuote(encoded) + " | base64 -d >\"$dir/command.sh\"\n" +
		"cat >\"$dir/runner.sh\" <<'SHW_RUNNER'\n" + runner + "SHW_RUNNER\n" +
		"nohup bash \"$dir/runner.sh\" </dev/null >/dev/null 2>&1 &\n" +
		"printf '%s\\n' \"$!\" >\"$dir/runner.pid\"\n" +
		"printf 'started'\n"
	result, err := r.command(ctx, job, profile, script)
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.Stdout) != "started" {
		return errors.New("远端未返回启动确认")
	}
	return nil
}

func (r *sshTaskRemote) Probe(ctx context.Context, job TaskJob, profile Profile) (string, error) {
	script := "dir=" + taskDir(job.ID) + "; if test -f \"$dir/exit.code\"; then printf 'done:%s' \"$(cat \"$dir/exit.code\")\"; elif test -f \"$dir/runner.pid\"; then pid=$(cat \"$dir/runner.pid\"); if kill -0 \"$pid\" 2>/dev/null; then printf running; else printf lost; fi; elif test -d \"$dir\"; then printf incomplete; else printf missing; fi"
	result, err := r.command(ctx, job, profile, script)
	return strings.TrimSpace(result.Stdout), err
}

func (r *sshTaskRemote) Logs(ctx context.Context, job TaskJob, profile Profile) (TaskLogs, error) {
	read := func(name string) (string, error) {
		script := "dir=" + taskDir(job.ID) + "; if test -f \"$dir/" + name + "\"; then tail -c 16384 \"$dir/" + name + "\"; fi"
		result, err := r.command(ctx, job, profile, script)
		return result.Stdout, err
	}
	stdout, err := read("stdout.log")
	if err != nil {
		return TaskLogs{}, err
	}
	stderr, err := read("stderr.log")
	return TaskLogs{Stdout: stdout, Stderr: stderr}, err
}

type archiveWriter struct {
	file *os.File
	size int64
}

func (w *archiveWriter) Write(p []byte) (int, error) {
	if w.size+int64(len(p)) > maxTaskArchive {
		return 0, errors.New("结果包超过 5 MiB 上限")
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (r *sshTaskRemote) Collect(ctx context.Context, job TaskJob, profile Profile, destination string) error {
	addr := net.JoinHostPort(job.Host, strconv.Itoa(job.Port))
	client, conn, err := dial(ctx, addr, profile, r.trust.Callback(addr, false))
	if err != nil {
		return err
	}
	defer client.Close()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	f, err := os.CreateTemp(filepath.Dir(destination), ".archive-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	writer := &archiveWriter{file: f}
	session.Stdout = writer
	var stderr boundedOutput
	session.Stderr = &stderr
	script := "dir=" + taskDir(job.ID) + "; tar -czf - -C \"$dir\" stdout.log stderr.log results"
	if err := session.Run("bash -o pipefail -c " + shellQuote(script)); err != nil {
		return fmt.Errorf("回收结果失败：%w：%s", err, stderr.b.String())
	}
	if writer.size == 0 {
		return io.ErrUnexpectedEOF
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), destination)
}

var _ taskRemote = (*sshTaskRemote)(nil)
