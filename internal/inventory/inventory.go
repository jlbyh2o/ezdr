// Package inventory has helpers shared by the client and portal for
// ezdr.inventory.v1.Inventory messages.
package inventory

import (
	"crypto/sha256"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
)

// Hash identifies an inventory's structure: guests, disks, NICs, storage,
// datasets, and network configuration. The collection time and usage figures
// (space used and free), which change constantly on a busy host, are
// excluded, so the hash changes only when something meaningful does.
func Hash(inv *inventoryv1.Inventory) []byte {
	c := proto.CloneOf(inv)
	c.CollectedAt = nil
	for _, s := range c.Storages {
		s.TotalBytes, s.UsedBytes, s.AvailableBytes = 0, 0, 0
	}
	for _, p := range c.ZfsPools {
		p.AllocatedBytes, p.FreeBytes, p.FragmentationPercent = 0, 0, 0
	}
	for _, d := range c.ZfsDatasets {
		d.UsedBytes, d.ReferencedBytes = 0, 0
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	sum := sha256.Sum256(b)
	return sum[:]
}

// NotReady counts guests that are not ready for replication.
func NotReady(inv *inventoryv1.Inventory) int {
	n := 0
	for _, g := range inv.GetGuests() {
		if !g.GetReady() {
			n++
		}
	}
	return n
}
