// Command server 是 NetDisk 的 HTTP 服务入口。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/config"
	"github.com/Miraitowa-kawayi/netdisk/internal/handler"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/Miraitowa-kawayi/netdisk/internal/service"
	"github.com/Miraitowa-kawayi/netdisk/internal/storage"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// SIGINT / SIGTERM 取消这个 context，触发下面的优雅退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := repository.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	// 启动时探测一次数据库，把"配置写错"这种问题尽早暴露出来。
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := store.Ping(pingCtx); err != nil {
		return err
	}

	// 内容存哪里：D1 只有 local，P6 会加 s3，上层只认 storage.Storage 接口。
	var blobStore storage.Storage
	switch cfg.StorageDriver {
	case "local":
		local, err := storage.NewLocal(cfg.StorageDir)
		if err != nil {
			return err
		}
		blobStore = local
	default:
		return fmt.Errorf("unsupported NETDISK_STORAGE_DRIVER %q", cfg.StorageDriver)
	}

	tokens := service.NewTokens(cfg.JWTSecret, cfg.JWTTTL)
	auth := service.NewAuth(store, tokens)
	files := service.NewFiles(store, blobStore, cfg.StorageDriver, logger)

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: handler.NewRouter(handler.Deps{
			Cfg:    cfg,
			Logger: logger,
			Store:  store,
			Auth:   auth,
			Files:  files,
		}),
		// 只限制读请求头的时间，防止慢速连接占坑；
		// 不设 ReadTimeout / WriteTimeout —— 上传下载都是大体积长连接，设了必然中途掐断。
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("netdisk listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}
