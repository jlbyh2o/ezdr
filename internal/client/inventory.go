package client

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/jlbyh2o/ezdr/internal/client/collect"
	"github.com/jlbyh2o/ezdr/internal/client/pve"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1/clientv1connect"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	"github.com/jlbyh2o/ezdr/internal/inventory"
)

// inventoryInterval is how often the client checks for inventory changes.
const inventoryInterval = time.Minute

// usageInterval is the longest the client goes without reporting, so usage
// figures (which don't count as changes) stay reasonably fresh.
const usageInterval = 15 * time.Minute

// inventoryLoop reports inventory when it starts, whenever it changes, and
// when the portal asks for a refresh (action IDs arrive on refresh). It runs
// until ctx is canceled.
func inventoryLoop(ctx context.Context, api clientv1connect.ClientServiceClient, refresh <-chan string) {
	var lastSent []byte
	var lastSentAt time.Time
	report := func(force bool) error {
		inv, err := collectInventory(ctx)
		if err != nil {
			return err
		}
		hash := inventory.Hash(inv)
		if !force && bytes.Equal(hash, lastSent) && time.Since(lastSentAt) < usageInterval {
			return nil
		}
		if _, err := api.ReportInventory(ctx, connect.NewRequest(&clientv1.ReportInventoryRequest{Inventory: inv})); err != nil {
			return fmt.Errorf("report inventory: %w", err)
		}
		lastSent, lastSentAt = hash, time.Now()
		slog.Info("inventory reported", "guests", len(inv.Guests), "not_ready", inventory.NotReady(inv),
			"warnings", len(inv.Warnings))
		return nil
	}

	if err := report(true); err != nil && ctx.Err() == nil {
		slog.Warn("inventory", "err", err)
	}
	ticker := time.NewTicker(inventoryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := report(false); err != nil && ctx.Err() == nil {
				slog.Warn("inventory", "err", err)
			}
		case id := <-refresh:
			err := report(true)
			ack := &clientv1.AckActionRequest{ActionId: id, Succeeded: err == nil}
			if err != nil {
				ack.Message = err.Error()
			}
			if _, aerr := api.AckAction(ctx, connect.NewRequest(ack)); aerr != nil && ctx.Err() == nil {
				slog.Warn("acknowledge action", "action", id, "err", aerr)
			}
		}
	}
}

// collectInventory collects from the local Proxmox VE API, creating or
// repairing the read-only API token if needed.
func collectInventory(ctx context.Context) (*inventoryv1.Inventory, error) {
	c, err := pve.NewClient()
	if err == nil {
		inv, cerr := collect.Collect(ctx, collect.LocalSources(c))
		if cerr == nil {
			return inv, nil
		}
		err = cerr
	}
	created, terr := pve.EnsureToken(ctx)
	if terr != nil {
		return nil, fmt.Errorf("%w (and could not set up the Proxmox API token: %w)", err, terr)
	}
	if !created {
		return nil, err
	}
	slog.Info("created read-only Proxmox API token", "token", pve.TokenID)
	if c, err = pve.NewClient(); err != nil {
		return nil, err
	}
	return collect.Collect(ctx, collect.LocalSources(c))
}
