// Command ezdr-portal runs the EZDR web portal.
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

	"github.com/jlbyh2o/ezdr/internal/portal"
	"github.com/jlbyh2o/ezdr/internal/version"
	"github.com/jlbyh2o/ezdr/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("portal exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", ":8080", "address to listen on")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ezdr-portal", version.String())
		return nil
	}

	if !web.Built {
		slog.Warn("web UI not embedded; serving placeholder page")
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           portal.NewHandler(web.FS()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		slog.Info("portal listening", "addr", *listen, "version", version.Version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
