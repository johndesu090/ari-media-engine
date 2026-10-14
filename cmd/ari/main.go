// Command ari runs the Ari Media Engine node: the WebRTC SFU, signaling
// endpoint, REST control plane, embeddable player and ingest listeners.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/johndesu090/ari-media-engine/internal/config"
	"github.com/johndesu090/ari-media-engine/internal/server"
	"github.com/johndesu090/ari-media-engine/pkg/logger"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to a YAML configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ari-media-engine", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ari: configuration error:", err)
		os.Exit(1)
	}

	log := logger.New(logger.Options{Level: cfg.Log.Level, Format: cfg.Log.Format})
	log.Info("starting ari media engine", "version", version)

	server.BuildVersion = version
	srv, err := server.New(cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
