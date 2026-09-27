package client

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"connectrpc.com/connect"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1/clientv1connect"
	"github.com/jlbyh2o/ezdr/internal/version"
)

var errStreamTimeout = errors.New("no message from the portal within the timeout")

// streamTimeout is how long the client waits for any message (the portal
// sends heartbeats every 15 seconds) before reconnecting.
const streamTimeout = 45 * time.Second

// Run is the long-running service: it keeps the tunnel configured and the
// command stream to the portal open.
func Run(ctx context.Context) error {
	cfg, err := LoadConfig(ConfigDir)
	if err != nil {
		return err
	}
	key, err := LoadKey(PrivateKeyFile)
	if err != nil {
		return err
	}

	// h2c: the tunnel is already encrypted by WireGuard.
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{Protocols: protocols}}
	api := clientv1connect.NewClientServiceClient(hc, cfg.ClientAPIURL)

	slog.Info("ezdr client starting", "version", version.Version, "portal", cfg.PortalURL,
		"tunnel_address", cfg.TunnelAddress.String())

	backoff := time.Second
	for {
		// Re-applying the interface also re-resolves the portal endpoint.
		if err := EnsureInterface(cfg, key); err != nil {
			slog.Error("set up tunnel", "err", err)
		} else if received, err := subscribe(ctx, api); ctx.Err() != nil {
			return nil
		} else {
			slog.Warn("command stream ended", "err", err)
			if received {
				backoff = time.Second
			}
		}

		wait := backoff/2 + rand.N(backoff/2+1) //nolint:gosec // jitter, not security
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// subscribe runs one command stream until it fails. It reports whether any
// message was received, so the caller can reset its backoff.
func subscribe(ctx context.Context, api clientv1connect.ClientServiceClient) (bool, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stream, err := api.Subscribe(ctx, connect.NewRequest(&clientv1.SubscribeRequest{ClientVersion: version.Version}))
	if err != nil {
		return false, err
	}
	defer func() { _ = stream.Close() }()

	watchdog := time.AfterFunc(streamTimeout, func() { cancel(errStreamTimeout) })
	defer watchdog.Stop()

	refresh := make(chan string, 1)
	go inventoryLoop(ctx, api, refresh)

	received := false
	for stream.Receive() {
		watchdog.Reset(streamTimeout)
		if !received {
			slog.Info("connected to portal")
			received = true
		}
		if a := stream.Msg().GetAction(); a != nil {
			handleAction(ctx, api, a, refresh)
		}
		if ds := stream.Msg().GetDesiredState(); ds != nil {
			// Phase 1 has nothing to apply; acknowledge the generation.
			_, err := api.ReportStatus(ctx, connect.NewRequest(&clientv1.ReportStatusRequest{
				ClientVersion: version.Version, AppliedGeneration: ds.Generation,
			}))
			if err != nil {
				slog.Warn("report status", "err", err)
			}
		}
	}
	if cause := context.Cause(ctx); errors.Is(cause, errStreamTimeout) {
		return received, cause
	}
	if err := stream.Err(); err != nil {
		return received, err
	}
	return received, errors.New("stream closed by portal")
}

// handleAction dispatches an action from the portal. Unknown kinds are
// rejected, so the client only ever performs operations it knows.
func handleAction(ctx context.Context, api clientv1connect.ClientServiceClient, a *clientv1.Action, refresh chan<- string) {
	fail := func(msg string) {
		_, err := api.AckAction(ctx, connect.NewRequest(&clientv1.AckActionRequest{ActionId: a.Id, Message: msg}))
		if err != nil {
			slog.Warn("acknowledge action", "action", a.Id, "err", err)
		}
	}
	switch a.Kind.(type) {
	case *clientv1.Action_RefreshInventory:
		select {
		case refresh <- a.Id:
		default:
			fail("an inventory refresh is already in progress")
		}
	default:
		slog.Warn("rejected unsupported action", "action", a.Id)
		fail("unsupported action; upgrade the ezdr client")
	}
}
