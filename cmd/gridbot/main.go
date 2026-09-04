// Command gridbot 是 dex-grid 的进程入口：加载配置、恢复实例、启动 REST API。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"dex-grid/internal/api"
	"dex-grid/internal/app/supervisor"
	"dex-grid/internal/config"
	"dex-grid/internal/exchange"
	"dex-grid/internal/exchange/rhlighter"
	"dex-grid/internal/exchange/sodex"
	"dex-grid/internal/infra/httpx"
	"dex-grid/internal/infra/lockfile"
	"dex-grid/internal/infra/logx"
	"dex-grid/internal/infra/store"

	_ "dex-grid/internal/domain/strategy/grid"
	_ "dex-grid/internal/domain/strategy/martingale"
	_ "dex-grid/internal/exchange/lighter"
)

func init() {
	// lighter 包 init 已占用 slot 0。后续 DEX 必须追加，不能插到中间。
	exchange.Register(rhlighter.Name, rhlighter.New)
	exchange.Register(sodex.Name, sodex.New)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gridbot: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "配置文件路径，默认 config/config.yaml")
	addr := flag.String("addr", "", "覆盖 HTTP 监听地址")
	flag.Parse()

	cfg, err := config.Load(resolveConfigPath(*configPath))
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}

	dataDir := config.ResolvePath(cfg.App.DataDir)
	lock, err := lockfile.Acquire(dataDir)
	if err != nil {
		return err
	}
	defer lock.Release()

	if cfg.App.LogFile != "" {
		logPath := config.ResolvePath(cfg.App.LogFile)
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return fmt.Errorf("创建日志目录失败: %w", err)
		}
		cfg.App.LogFile = logPath
	}
	buf, err := logx.Setup(cfg.App.LogLevel, cfg.App.LogFormat, cfg.App.LogFile, cfg.App.LogBufferSize)
	if err != nil {
		return err
	}
	log := slog.Default()

	st, err := store.Open(filepath.Join(dataDir, "gridbot.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	httpClient, err := httpx.New(cfg.Proxy, cfg.App.HTTPTimeout.Std())
	if err != nil {
		return err
	}
	if httpClient.Timeout <= 0 {
		httpClient.Timeout = 15 * time.Second
	}

	sup := supervisor.New(cfg, st, buf, log)
	for _, e := range cfg.EnabledExchanges() {
		slot, err := exchange.Slot(e.Name)
		if err != nil {
			return err
		}
		ex, err := exchange.New(e, exchange.Deps{Log: log, HTTP: httpClient})
		if err != nil {
			return fmt.Errorf("初始化交易所 %s 失败: %w", e.Name, err)
		}
		stream, ok := ex.(exchange.StreamingExchange)
		if !ok {
			_ = ex.Close()
			return fmt.Errorf("交易所 %s 没有事件流能力，无法运行网格", e.Name)
		}
		if !ex.Capabilities().PostOnly {
			_ = ex.Close()
			return fmt.Errorf("交易所 %s 不支持 post-only，无法运行网格", e.Name)
		}
		sup.Attach(e, stream, slot)
		log.Info("exchange attached", "exchange", e.Name, "slot", slot, "network", e.Network)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := sup.LoadStrategyFiles(ctx); err != nil {
		return err
	}
	sup.RestoreRunning(ctx)
	sup.Autostart(ctx)

	srv := api.New(sup, cfg.Server)
	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.Server.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.App.ShutdownTimeout.Std())
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	sup.Close(shutCtx)
	return nil
}

// resolveConfigPath 默认使用 config/config.yaml，相对路径按可执行文件目录解析，
// 找不到再回退到当前工作目录，这样直接运行 gridbot.exe 不必再带 -config。
func resolveConfigPath(explicit string) string {
	if explicit != "" {
		if filepath.IsAbs(explicit) {
			return explicit
		}
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		return config.ResolvePath(explicit)
	}
	for _, p := range []string{
		config.ResolvePath("config/config.yaml"),
		"config/config.yaml",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return config.ResolvePath("config/config.yaml")
}
