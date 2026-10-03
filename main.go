package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	notifier = newMailer(cfg)
	go runPlanner(context.Background(), cfg)
	go runScheduler(context.Background(), cfg)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           webMux(cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("listening", "addr", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return mux
}
