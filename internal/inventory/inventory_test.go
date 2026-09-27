package inventory

import (
	"bytes"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
)

func TestHashIgnoresCollectionTime(t *testing.T) {
	a := &inventoryv1.Inventory{
		CollectedAt: timestamppb.New(time.Unix(1, 0)),
		Guests:      []*inventoryv1.Guest{{Vmid: 101, Ready: true}, {Vmid: 102}},
	}
	b := &inventoryv1.Inventory{
		CollectedAt: timestamppb.New(time.Unix(2, 0)),
		Guests:      []*inventoryv1.Guest{{Vmid: 101, Ready: true}, {Vmid: 102}},
	}
	if !bytes.Equal(Hash(a), Hash(b)) {
		t.Error("collection time changed the hash")
	}
	if a.CollectedAt == nil {
		t.Error("Hash modified its argument")
	}
	b.Guests[1].Name = "changed"
	if bytes.Equal(Hash(a), Hash(b)) {
		t.Error("content change did not change the hash")
	}
	if NotReady(a) != 1 {
		t.Errorf("NotReady = %d, want 1", NotReady(a))
	}
}

func TestHashIgnoresUsage(t *testing.T) {
	mk := func(used uint64) *inventoryv1.Inventory {
		return &inventoryv1.Inventory{
			Storages:    []*inventoryv1.Storage{{Id: "local-zfs", UsedBytes: used, AvailableBytes: 1000 - used}},
			ZfsPools:    []*inventoryv1.ZfsPool{{Name: "rpool", AllocatedBytes: used}},
			ZfsDatasets: []*inventoryv1.ZfsDataset{{Name: "rpool/data", UsedBytes: used, ReferencedBytes: used}},
		}
	}
	if !bytes.Equal(Hash(mk(100)), Hash(mk(200))) {
		t.Error("usage figures changed the hash")
	}
	a, b := mk(100), mk(100)
	b.ZfsDatasets[0].Name = "rpool/other"
	if bytes.Equal(Hash(a), Hash(b)) {
		t.Error("structural change did not change the hash")
	}
}
