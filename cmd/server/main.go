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

	// SIGINT / SIGTERM 取消 context，触发优雅退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := repository.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	// 启动时探测数据库，尽早暴露配置错误。
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := store.Ping(pingCtx); err != nil {
		return err
	}

	// 内容存储后端：上层只依赖 storage.Storage 接口。
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
	uploads := service.NewUploads(store, blobStore, cfg.StorageDriver, logger)
	shares := service.NewShares(store, files, logger)

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: handler.NewRouter(handler.Deps{
			Cfg:     cfg,
			Logger:  logger,
			Store:   store,
			Auth:    auth,
			Files:   files,
			Uploads: uploads,
			Shares:  shares,
		}),
		// 只设 ReadHeaderTimeout；上传下载是大体积长连接，Read/WriteTimeout 会中途掐断。
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
