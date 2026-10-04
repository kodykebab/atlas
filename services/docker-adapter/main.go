// command atlas-docker-adapter maps docker requests to atlas vms.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/frappe/atlas/services/docker-adapter/internal/atlas"
	"github.com/frappe/atlas/services/docker-adapter/internal/engine"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], logger); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		logger.Error("adapter stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, logger *slog.Logger) error {
	flags := flag.NewFlagSet("atlas-docker-adapter", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:2375", "address for Docker clients; use loopback or terminate TLS in front")
	atlasURL := flags.String("atlas-url", "", "Atlas base URL")
	cpu := flags.Int("cpu-millicores", 1000, "default VM CPU")
	memory := flags.Int("memory-mib", 1024, "default VM memory")
	disk := flags.Int("disk-mib", 10240, "VM root disk size")
	interval := flags.Duration("poll-interval", time.Second, "interval between Atlas state checks")
	timeout := flags.Duration("operation-timeout", 5*time.Minute, "deadline for each request except wait")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	upstream, err := url.Parse(*atlasURL)
	if err != nil || upstream.Hostname() == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" || (upstream.Path != "" && upstream.Path != "/") {
		return fmt.Errorf("-atlas-url must be an HTTP(S) origin without credentials, query or path")
	}
	if *cpu < 100 || *cpu > 32000 || *memory <= 0 || *disk <= 0 {
		return fmt.Errorf("CPU must be 100..32000 millicores; memory and disk must be positive MiB")
	}
	if *interval <= 0 || *timeout <= 0 {
		return fmt.Errorf("poll interval and operation timeout must be positive")
	}
	handler := engine.NewServer(engine.Config{
		Atlas:                atlas.NewClient(*atlasURL, 30*time.Second),
		DefaultCPUMillicores: *cpu, DefaultMemoryMiB: *memory, DiskMiB: *disk,
		PollInterval: *interval, OperationTimeout: *timeout, Logger: logger,
	})
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.Info("adapter listening", "address", listener.Addr().String(), "atlas", *atlasURL)
	return serve(ctx, listener, handler)
}

func serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	finished := make(chan struct{})
	shutdownResult := make(chan error, 1)
	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			server.Close()
		}
		shutdownResult <- err
	}()
	err := server.Serve(listener)
	close(finished)
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if err := <-shutdownResult; err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
