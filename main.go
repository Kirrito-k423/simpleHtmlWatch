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

func main() {
	if err := start(); err != nil {
		fmt.Fprintln(os.Stderr, "simpleHtmlWatch:", err)
		os.Exit(1)
	}
}
func start() error {
	port := flag.Int("port", 0, "首次自动选端口，后续复用登记端口；显式 0 重新选端口")
	dir := flag.String("data-dir", "", "配置目录，默认用户配置目录下 simpleHtmlWatch")
	instance := flag.String("instance", "default", "实例名；不同实例使用独立数据目录")
	status := flag.Bool("status", false, "只读发现实例并验证身份，输出 JSON")
	adoptBuild := flag.Bool("adopt-build", false, "服务停止后，明确将此构建选为该实例的运行版本")
	noBrowser := flag.Bool("no-browser", false, "不自动打开浏览器")
	showVersion := flag.Bool("version", false, "显示版本")
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
	lease, existing, err := watch.AcquireService(*dir, *instance, version, *port, explicitPort, *adoptBuild)
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
	store, err := watch.NewStore(*dir)
	if err != nil {
		return err
	}
	trust, err := watch.NewTrustStore(*dir)
	if err != nil {
		return err
	}
	listener := lease.Listener
	history, err := watch.NewHistory(*dir)
	if err != nil {
		return err
	}
	defer history.Close()
	monitor := watch.NewMonitor(trust)
	monitor.SetHistory(history)
	monitor.Replace(store.Snapshot())
	defer monitor.Close()
	web, _ := fs.Sub(assets, "web")
	app := watch.NewServer(store, monitor, trust, web, listener.Addr().String())
	if err := app.EnableTasks(); err != nil {
		return err
	}
	defer app.Close()
	app.SetServiceInfo(lease.Info)
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
