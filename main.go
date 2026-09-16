package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
	port := flag.Int("port", 0, "本机端口，0 自动选择空闲端口")
	dir := flag.String("data-dir", "", "配置目录，默认用户配置目录下 simpleHtmlWatch")
	noBrowser := flag.Bool("no-browser", false, "不自动打开浏览器")
	showVersion := flag.Bool("version", false, "显示版本")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		*dir = filepath.Join(base, "simpleHtmlWatch")
	}
	unlock, err := watch.LockDirectory(*dir)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := watch.NewStore(*dir)
	if err != nil {
		return err
	}
	trust, err := watch.NewTrustStore(*dir)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	defer listener.Close()
	monitor := watch.NewMonitor(trust)
	monitor.Replace(store.Snapshot())
	defer monitor.Close()
	web, _ := fs.Sub(assets, "web")
	app := watch.NewServer(store, monitor, trust, web, listener.Addr().String())
	server := &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	url := "http://" + listener.Addr().String()
	fmt.Printf("\nsimpleHtmlWatch %s\n\n监控页面：%s\n本机配置：%s\n保持此窗口运行，按 Ctrl+C 退出。\n\n", version, url, *dir)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
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
