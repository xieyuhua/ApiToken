// apitoken：多平台大模型 API 网关（DeepSeek / 商汤日日新 / 腾讯 WorkBuddy 等）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/gateway"
	"github.com/demo1/apitoken/internal/store"
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
		logger.Info("apitoken 网关已启动",
			"version", gateway.Version,
			"addr", "http://127.0.0.1"+cfg.Server.Addr,
			"ui", "http://127.0.0.1"+cfg.Server.Addr+"/",
			"api", "http://127.0.0.1"+cfg.Server.Addr+"/v1",
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
