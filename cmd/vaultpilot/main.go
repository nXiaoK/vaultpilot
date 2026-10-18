package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vaultpilot/internal/engine"
	"vaultpilot/internal/platform"
)

const version = "0.1.0"

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func roots(key, fallback string) []string {
	values := strings.Split(env(key, fallback), ",")
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
func run() error {
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println("VaultPilot", version)
		return nil
	}
	concurrency, err := strconv.Atoi(env("VP_GLOBAL_CONCURRENCY", "2"))
	if err != nil {
		return fmt.Errorf("VP_GLOBAL_CONCURRENCY 必须为整数: %w", err)
	}
	logMaxRows, err := strconv.Atoi(env("VP_LOG_MAX_ROWS", "100000"))
	if err != nil {
		return fmt.Errorf("VP_LOG_MAX_ROWS 必须为整数: %w", err)
	}
	cacheMaxBytes, err := strconv.ParseInt(env("VP_CACHE_MAX_BYTES", "21474836480"), 10, 64)
	if err != nil {
		return fmt.Errorf("VP_CACHE_MAX_BYTES 必须为整数: %w", err)
	}
	config := platform.Config{
		Addr: env("VP_ADDR", "127.0.0.1:8080"), StateDir: env("VP_STATE_DIR", "./state"),
		CacheDir: env("VP_CACHE_DIR", "./cache"), PublicURL: env("VP_PUBLIC_URL", "http://localhost:8080"),
		ResticBin: env("VP_RESTIC_BIN", "restic"), RcloneBin: env("VP_RCLONE_BIN", "rclone"),
		AutoUnlockFile: os.Getenv("VP_AUTO_UNLOCK_FILE"), SourceRoots: roots("VP_SOURCE_ROOTS", "/sources"),
		AllowedHosts: roots("VP_ALLOWED_HOSTS", ""), RestoreRoots: roots("VP_RESTORE_ROOTS", "/restore"), AllowLocal: os.Getenv("VP_ALLOW_LOCAL") == "true",
		GlobalConcurrency: concurrency, LogMaxRows: logMaxRows, CacheMaxBytes: cacheMaxBytes,
		MaintenanceWindow: os.Getenv("VP_MAINTENANCE_WINDOW"), MaintenanceTimezone: env("VP_MAINTENANCE_TIMEZONE", "Asia/Shanghai"),
		OAuthClientID: os.Getenv("VP_OAUTH_CLIENT_ID"), OAuthClientSecret: os.Getenv("VP_OAUTH_CLIENT_SECRET"),
		OAuthTenant: env("VP_OAUTH_TENANT", "consumers"),
	}
	if err := os.MkdirAll(config.StateDir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(config.StateDir, ".instance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("该状态目录已有运行中的 VaultPilot 节点: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	runtime, err := platform.New(config)
	if err != nil {
		return err
	}
	defer runtime.Close()
	service, err := engine.New(runtime)
	if err != nil {
		return err
	}
	defer service.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mux := http.NewServeMux()
	runtime.Register(mux)
	service.Register(mux)
	server := &http.Server{Addr: config.Addr, Handler: runtime.Handler(mux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	service.Start(ctx)
	errCh := make(chan error, 1)
	go func() {
		log.Printf("VaultPilot %s listening on %s", version, config.Addr)
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
