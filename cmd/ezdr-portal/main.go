// Command ezdr-portal runs the EZDR web portal.
//
// Configuration is read from environment variables; see the deployment
// documentation for the full list.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jlbyh2o/ezdr/internal/portal"
	"github.com/jlbyh2o/ezdr/internal/version"
	"github.com/jlbyh2o/ezdr/web"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("ezdr-portal", version.String())
		return
	}

	cfg, err := portal.LoadConfig(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	if !web.Built {
		slog.Warn("web UI not embedded; serving placeholder page")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := portal.Run(ctx, cfg, web.FS()); err != nil {
		slog.Error("portal exited", "err", err)
		os.Exit(1)
	}
}
