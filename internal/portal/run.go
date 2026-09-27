package portal

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jlbyh2o/ezdr/internal/portal/api"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/portal/tunnel"
	"github.com/jlbyh2o/ezdr/internal/version"
)

// TunnelAPIPort is the client API's TCP port on the portal's tunnel address.
const TunnelAPIPort = 8080

// wireguardKeySecret names the secrets-table row holding the portal's key.
const wireguardKeySecret = "wireguard_private_key" //nolint:gosec // a row name, not a credential

// Run starts the portal and blocks until ctx is canceled or a server fails.
func Run(ctx context.Context, cfg Config, ui fs.FS) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	key, err := LoadOrCreateSecretKey(cfg.SecretKeyFile)
	if err != nil {
		return fmt.Errorf("load secret key: %w", err)
	}
	box, err := store.NewSecretBox(key)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "ezdr.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	tun, err := startTunnel(ctx, cfg, st, box)
	if err != nil {
		return err
	}
	defer tun.Close()

	d := &api.Deps{
		Store: st, Box: box, Peers: tun, Hub: api.NewHub(),
		PublicURL: cfg.PublicURL, TLSPin: cfg.TLSPin, SecureCookies: cfg.SecureCookies(),
		WireGuardEndpoint: cfg.WireGuardEndpoint, TunnelPrefix: cfg.TunnelPrefix, SiteTunnelPrefix: cfg.SiteTunnelPrefix,
		PortalTunnelAddr: cfg.PortalTunnelAddress(),
		ClientAPIURL: "http://" + net.JoinHostPort(cfg.PortalTunnelAddress().String(),
			strconv.Itoa(TunnelAPIPort)),
		Setup: api.NewSetupCode(),
	}
	if n, err := st.CountUsers(ctx); err != nil {
		return err
	} else if n == 0 {
		slog.Warn("no users yet: open the portal to create the first administrator",
			"url", cfg.PublicURL.String(), "setup_code", d.Setup.String())
	}

	public := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.WithSourceAddress(cfg.TrustedProxies, PublicHandler(d, ui)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if cfg.TLS == TLSSelfSigned {
		cert, pin, err := LoadOrCreateSelfSigned(cfg.DataDir, cfg.PublicURL.Hostname())
		if err != nil {
			return fmt.Errorf("self-signed certificate: %w", err)
		}
		d.TLSPin = pin
		public.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		slog.Info("serving HTTPS with a self-signed certificate", "pin", pin)
	}

	tunnelLn, err := tun.Listen(TunnelAPIPort)
	if err != nil {
		return fmt.Errorf("listen on tunnel: %w", err)
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // WireGuard already encrypts the tunnel
	private := &http.Server{
		Handler:           TunnelHandler(d),
		ReadHeaderTimeout: 10 * time.Second,
		Protocols:         protocols,
		// No read/write timeouts: command streams are long-lived.
	}

	errc := make(chan error, 2)
	go func() {
		slog.Info("portal listening", "addr", cfg.Listen, "version", version.Version,
			"tunnel", cfg.PortalTunnelAddress().String(), "wireguard_port", cfg.WireGuardPort)
		if public.TLSConfig != nil {
			errc <- public.ListenAndServeTLS("", "")
		} else {
			errc <- public.ListenAndServe()
		}
	}()
	go func() { errc <- private.Serve(tunnelLn) }()
	go cleanupSessions(ctx, st)
	go api.NewAlertEngine(d).Run(ctx)
	go d.ResumeTakeovers(ctx)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Close the tunnel server first so command streams end promptly.
	_ = private.Close()
	if err := public.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startTunnel starts WireGuard with the portal's persistent key and adds a
// peer for every enrolled host.
func startTunnel(ctx context.Context, cfg Config, st *store.Store, box *store.SecretBox) (*tunnel.Tunnel, error) {
	var privateKey []byte
	sealed, err := st.Secret(ctx, wireguardKeySecret)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if privateKey, err = tunnel.GeneratePrivateKey(); err != nil {
			return nil, err
		}
		if err := st.PutSecret(ctx, wireguardKeySecret, box.Seal(privateKey)); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if privateKey, err = box.Open(sealed); err != nil {
			return nil, fmt.Errorf("decrypt WireGuard key (wrong secret key file?): %w", err)
		}
	}

	tun, err := tunnel.Start(privateKey, cfg.PortalTunnelAddress(), cfg.WireGuardPort)
	if err != nil {
		return nil, err
	}
	hosts, err := st.ListHosts(ctx)
	if err != nil {
		tun.Close()
		return nil, err
	}
	for _, h := range hosts {
		if err := tun.AddPeer(h.WireGuardPublicKey, h.TunnelAddress); err != nil {
			tun.Close()
			return nil, fmt.Errorf("add peer for host %s: %w", h.ID, err)
		}
	}
	return tun, nil
}

func cleanupSessions(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.DeleteExpiredSessions(ctx); err != nil && ctx.Err() == nil {
				slog.Error("delete expired sessions", "err", err)
			}
		}
	}
}
