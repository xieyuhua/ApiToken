// apitoken：多平台大模型 API 网关（DeepSeek / 商汤日日新 / 腾讯 WorkBuddy 等）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/gateway"
	"github.com/xieyuhua/apitoken/internal/store"
)

func main() {
	var (
		cfgPath = flag.String("config", "config.yaml", "配置文件路径")
		addr    = flag.String("addr", "", "覆盖监听地址，如 :9000")
		showVer = flag.Bool("version", false, "打印版本号")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(gateway.Version)
		return
	}

	logger := newLogger("info")

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}
	logger = newLogger(cfg.Logging.Level)

	st, err := store.New(cfg, logger)
	if err != nil {
		logger.Error("初始化存储失败", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           gateway.New(cfg, st, logger).Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.D(),
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		display := displayAddr(cfg.Server.Addr)
		logger.Info("apitoken 网关已启动",
			"version", gateway.Version,
			"addr", "http://"+display,
			"ui", "http://"+display+"/",
			"api", "http://"+display+"/v1",
			"channels", len(st.ListChannels()),
			"config", cfg.Path(),
			"data", cfg.DataFile(),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务异常退出", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("收到退出信号，正在关闭…")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("关闭超时", "err", err)
	}
	// HTTP 已停止接受请求，此时 flush 日志管道，避免丢掉最后一批访问日志
	st.CloseLogs()
	logger.Info("已退出")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

// displayAddr 把监听地址规范为日志里可直接访问的展示地址：
// ":8080" 与 "0.0.0.0:8080" 统一显示为 127.0.0.1:8080，具体 IP 原样展示。
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr // 非 host:port 形式，原样返回
	}
	if host == "" || host == "0.0.0.0" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}
