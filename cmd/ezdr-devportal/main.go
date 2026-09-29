// Command ezdr-devportal runs a portal with simulated hosts and fake data,
// for developing the web UI without real Proxmox VE hosts.
//
// It serves the API on 127.0.0.1:8080 for the Vite dev server (cd web &&
// pnpm dev) and opens http://localhost:5173/dev/signin to sign in. Nothing
// it does reaches a real host. See CONTRIBUTING.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jlbyh2o/ezdr/internal/portal"
	"github.com/jlbyh2o/ezdr/internal/portal/api"
	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/version"
	"github.com/jlbyh2o/ezdr/web"
)

func main() {
	dataDir := flag.String("data", ".devportal", "data directory (created and seeded if empty)")
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve on")
	reset := flag.Bool("reset", false, "delete the data directory and seed it again")
	fast := flag.Bool("fast", false, "replicate every 20 seconds instead of every minute")
	empty := flag.Bool("empty", false, "seed only the administrator, no hosts or plans")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *dataDir, *listen, *reset, *fast, *empty); err != nil {
		slog.Error("dev portal exited", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dataDir, listen string, reset, fast, empty bool) error {
	// Claim the port first, so a second instance fails before touching the
	// data directory.
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if reset {
		if err := os.RemoveAll(dataDir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	key, err := portal.LoadOrCreateSecretKey(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		return err
	}
	box, err := store.NewSecretBox(key)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(dataDir, "ezdr.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// Tokens embed the public URL and need HTTPS; nothing ever enrolls with
	// them here.
	publicURL, _ := url.Parse("https://portal.example.com")
	d := &api.Deps{
		Store: st, Box: box, Peers: noPeers{}, Hub: api.NewHub(),
		PublicURL: publicURL, SecureCookies: false,
		WireGuardEndpoint: "portal.example.com:51820",
		TunnelPrefix:      portal.DefaultTunnelPrefix, SiteTunnelPrefix: portal.DefaultSiteTunnelPrefix,
		PortalTunnelAddr: portal.DefaultTunnelPrefix.Addr().Next(),
		ClientAPIURL:     "http://100.64.42.1:8080",
		Setup:            api.NewSetupCode(),
	}

	n, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	fresh := n == 0
	if fresh {
		if err := seed(ctx, d, empty); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	pace := api.SimOptions{Cycle: time.Minute, Transfer: 12 * time.Second, ActionDelay: 1500 * time.Millisecond,
		ClientVersion: version.Version, StateFile: filepath.Join(dataDir, "sim.json")}
	if fast {
		pace.Cycle = 20 * time.Second
	}
	sim := api.NewSim(d, pace)
	saved := make(chan struct{})
	go func() {
		sim.Save(ctx)
		close(saved)
	}()
	defer func() { <-saved }()
	if err := startHosts(ctx, d, sim); err != nil {
		return err
	}
	portal.StartBackground(ctx, d)
	if fresh && !empty {
		go func() {
			if err := scenario(ctx, d); err != nil && ctx.Err() == nil {
				slog.Error("scenario", "err", err)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /dev/signin", func(w http.ResponseWriter, r *http.Request) { signIn(w, r, d) })
	mux.Handle("/", portal.PublicHandler(d, web.FS()))
	// Requests come through the Vite dev server on the same machine.
	local := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	srv := &http.Server{Addr: listen, Handler: api.WithSourceAddress(local, mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("dev portal listening", "addr", listen, "data", dataDir,
		"signin", "http://localhost:5173/dev/signin (through pnpm dev)")
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// signIn starts a session for the seeded administrator without a password
// or TOTP code.
func signIn(w http.ResponseWriter, r *http.Request, d *api.Deps) {
	u, err := d.Store.UserByUsername(r.Context(), seedUser)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, hash := auth.NewSessionID()
	if err := d.Store.CreateSession(r.Context(), hash, u.ID, time.Now().Add(auth.SessionTTL)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Add("Set-Cookie", auth.SessionCookieValue(id, false))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// noPeers stands in for WireGuard: simulated hosts connect in-process.
type noPeers struct{}

func (noPeers) PublicKey() []byte                    { return make([]byte, 32) }
func (noPeers) AddPeer(_ []byte, _ netip.Addr) error { return nil }
func (noPeers) RemovePeer(_ []byte) error            { return nil }
