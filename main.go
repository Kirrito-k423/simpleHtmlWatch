package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/Kirrito-k423/simpleHtmlWatch/internal/watch"
)

//go:embed web/*
var assets embed.FS
var version = "dev"

type logPaths []string

func (p *logPaths) String() string         { return fmt.Sprint([]string(*p)) }
func (p *logPaths) Set(value string) error { *p = append(*p, value); return nil }

func main() {
	if err := start(); err != nil {
		fmt.Fprintln(os.Stderr, "simpleHtmlWatch:", err)
		os.Exit(1)
	}
}
func start() error {
	var boundedLogs logPaths
	flag.Var(&boundedLogs, "bounded-log", "明确选择数据目录内的诊断 .log；可重复，最多 8 个；每 30 秒超过 64 KiB 时仅保留末尾 32 KiB")
	port := flag.Int("port", 0, "首次自动选端口，后续复用登记端口；显式 0 重新选端口")
	dir := flag.String("data-dir", "", "配置目录，默认用户配置目录下 simpleHtmlWatch")
	instance := flag.String("instance", "default", "实例名；不同实例使用独立数据目录")
	status := flag.Bool("status", false, "只读发现实例并验证身份，输出 JSON")
	adoptBuild := flag.Bool("adopt-build", false, "服务停止后，明确将此构建选为该实例的运行版本")
	noBrowser := flag.Bool("no-browser", false, "不自动打开浏览器")
	showVersion := flag.Bool("version", false, "显示版本")
	var backup watch.BackupCommand
	flag.StringVar(&backup.ArchiveDir, "backup-archive-dir", "", "外置升级备份仓库；首次配置后持久化，断开时保留本机回滚")
	flag.StringVar(&backup.Source, "archive-backup", "", "归档指定的旧备份目录；不会启动服务")
	flag.StringVar(&backup.SourceRoot, "backup-source-root", "", "明确允许归档来源所在的父目录")
	flag.BoolVar(&backup.Remove, "remove-archived-source", false, "归档认证校验和实际恢复演练通过后删除该源目录")
	flag.StringVar(&backup.Verify, "verify-backup", "", "解密并逐文件校验指定 .shwb 归档")
	flag.StringVar(&backup.Restore, "restore-backup", "", "恢复指定 .shwb 到新的恢复目录")
	flag.StringVar(&backup.RestoreDir, "restore-dir", "", "必须不存在的恢复目录；不会覆盖实例")
	flag.BoolVar(&backup.Status, "backup-status", false, "输出升级备份策略和回滚状态；不会启动服务")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	resolved, err := watch.ResolveServiceDir(*dir, *instance)
	if err != nil {
		return err
	}
	*dir = resolved
	if backup.Source != "" || backup.Verify != "" || backup.Restore != "" || backup.Status || backup.Remove || backup.SourceRoot != "" || backup.RestoreDir != "" {
		modes := 0
		for _, yes := range []bool{backup.Source != "", backup.Verify != "", backup.Restore != "", backup.Status} {
			if yes {
				modes++
			}
		}
		if modes != 1 {
			return fmt.Errorf("一次只能选择一个备份维护操作")
		}
		result, err := watch.RunBackupCommand(*dir, backup)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if *status {
		info, err := watch.DiscoverService(*dir, *instance)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	explicitPort := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			explicitPort = true
		}
	})
	lease, existing, err := watch.AcquireService(*dir, *instance, version, *port, explicitPort, *adoptBuild, backup.ArchiveDir)
	if err != nil {
		return err
	}
	if existing != nil {
		fmt.Printf("实例 %s 已运行，复用 %s（运行版本 %s，当前启动器 %s）；没有重启或切换版本。\n", existing.Instance, existing.URL, existing.Version, version)
		if !*noBrowser {
			openBrowser(existing.URL)
		}
		return nil
	}
	defer lease.Close()
	closeBackups, err := watch.StartBackupMaintenance(*dir, lease.Info)
	if err != nil {
		return err
	}
	defer closeBackups()
	logs, err := watch.NewBoundedLogs(*dir, boundedLogs)
	if err != nil {
		return err
	}
	defer logs.Close()
	store, err := watch.NewStore(*dir)
	if err != nil {
		return err
	}
	trust, err := watch.NewTrustStore(*dir)
	if err != nil {
		return err
	}
	listener := lease.Listener
	utilization, err := watch.NewUtilization(*dir)
	if err != nil {
		return err
	}
	utilization.Start()
	defer utilization.Close()
	monitor := watch.NewMonitor(trust)
	monitor.SetUtilization(utilization)
	monitor.Replace(store.Snapshot())
	defer monitor.Close()
	web, _ := fs.Sub(assets, "web")
	app := watch.NewServer(store, monitor, trust, web, listener.Addr().String())
	if err := app.EnableTasks(); err != nil {
		return err
	}
	defer app.Close()
	app.SetServiceInfo(lease.Info)
	if err := app.EnableIdleRanking(); err != nil {
		return err
	}
	if err := lease.Publish(); err != nil {
		return err
	}
	server := &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	url := lease.Info.URL
	fmt.Printf("\nsimpleHtmlWatch %s\n\n监控页面：%s\n本机配置：%s\n实例：%s\n发现文件：%s/service.json\n保持此窗口运行，按 Ctrl+C 退出。\n\n", version, url, *dir, *instance, *dir)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if !*noBrowser {
		go openBrowser(url)
	}
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		c = exec.Command("open", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Run()
}
