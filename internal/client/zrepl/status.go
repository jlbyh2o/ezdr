package zrepl

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// rawStatus is the part of `zrepl status --mode raw` EZDR reads.
type rawStatus struct {
	Jobs map[string]struct {
		Type string `json:"type"`
		Pull *struct {
			Replication *struct {
				WaitReconnectError any `json:"WaitReconnectError"`
				Attempts           []struct {
					State       string    `json:"State"`
					StartAt     time.Time `json:"StartAt"`
					FinishAt    time.Time `json:"FinishAt"`
					PlanError   any       `json:"PlanError"`
					Filesystems []struct {
						Info struct {
							Name string `json:"Name"`
						} `json:"Info"`
						State     string `json:"State"`
						PlanError any    `json:"PlanError"`
						StepError any    `json:"StepError"`
						Steps     []struct {
							Info struct {
								// From is empty for a full send.
								From            string `json:"From"`
								BytesExpected   uint64 `json:"BytesExpected"`
								BytesReplicated uint64 `json:"BytesReplicated"`
							} `json:"Info"`
						} `json:"Steps"`
					} `json:"Filesystems"`
				} `json:"Attempts"`
			} `json:"Replication"`
			PruningSender   *pruning `json:"PruningSender"`
			PruningReceiver *pruning `json:"PruningReceiver"`
		} `json:"pull"`
	} `json:"Jobs"`
}

type pruning struct {
	Error     string `json:"Error"`
	Completed []struct {
		Filesystem string `json:"Filesystem"`
		LastError  string `json:"LastError"`
	} `json:"Completed"`
}

// errorText extracts a message from zrepl's error encodings (null, a string,
// or an object with an "Err" field).
func errorText(v any) string {
	switch e := v.(type) {
	case nil:
		return ""
	case string:
		return e
	case map[string]any:
		if s, ok := e["Err"].(string); ok {
			return s
		}
		b, _ := json.Marshal(e)
		return string(b)
	default:
		return fmt.Sprint(e)
	}
}

// Status reports the state of EZDR's pull jobs in z: zrepl's view of the
// latest replication attempt, and the newest replicated snapshot of each
// dataset from ZFS itself.
func (a *Applier) Status(ctx context.Context, z *clientv1.Zrepl) *clientv1.ReportReplicationRequest {
	req := &clientv1.ReportReplicationRequest{CollectedAt: timestamppb.New(a.Now())}
	if len(z.GetPullJobs()) == 0 {
		return req
	}
	out, err := a.Run(ctx, "zrepl", "status", "--mode", "raw")
	if err != nil {
		req.Error = "read zrepl status: " + err.Error()
		return req
	}
	var raw rawStatus
	if err := json.Unmarshal(out, &raw); err != nil {
		req.Error = "parse zrepl status: " + err.Error()
		return req
	}

	for _, pj := range z.GetPullJobs() {
		js := &clientv1.JobStatus{Name: pj.Name, Type: "pull"}
		datasets := map[string]*clientv1.DatasetStatus{}
		ds := func(name string) *clientv1.DatasetStatus {
			if datasets[name] == nil {
				datasets[name] = &clientv1.DatasetStatus{Dataset: name}
			}
			return datasets[name]
		}

		job, ok := raw.Jobs[pj.Name]
		switch {
		case !ok || job.Pull == nil:
			js.Errors = append(js.Errors, "zrepl isn't running this job")
		default:
			if r := job.Pull.Replication; r != nil {
				if e := errorText(r.WaitReconnectError); e != "" {
					js.Errors = append(js.Errors, "connection: "+e)
				}
				if n := len(r.Attempts); n > 0 {
					at := r.Attempts[n-1]
					js.State = at.State
					js.AttemptStartedAt = timestamppb.New(at.StartAt)
					if !at.FinishAt.IsZero() && at.FinishAt.Year() > 1 {
						js.AttemptFinishedAt = timestamppb.New(at.FinishAt)
					}
					if e := errorText(at.PlanError); e != "" {
						js.Errors = append(js.Errors, "planning: "+e)
					}
					for _, f := range at.Filesystems {
						d := ds(f.Info.Name)
						d.State = f.State
						d.Error = strings.TrimSpace(errorText(f.PlanError) + " " + errorText(f.StepError))
						for _, s := range f.Steps {
							d.FullSend = d.FullSend || s.Info.From == ""
							d.BytesExpected += s.Info.BytesExpected
							d.BytesReplicated += s.Info.BytesReplicated
						}
					}
				}
			}
			for side, p := range map[string]*pruning{"primary": job.Pull.PruningSender, "DR host": job.Pull.PruningReceiver} {
				if p == nil {
					continue
				}
				if p.Error != "" {
					js.Errors = append(js.Errors, "pruning on the "+side+": "+p.Error)
				}
				for _, c := range p.Completed {
					if c.LastError != "" {
						js.Errors = append(js.Errors, fmt.Sprintf("pruning %s on the %s: %s", c.Filesystem, side, c.LastError))
					}
				}
			}
		}

		// Newest replicated snapshot per dataset, from ZFS.
		if snaps, err := a.latestSnapshots(ctx, pj.ReceiveDataset, pj.SnapshotPrefix); err != nil {
			js.Errors = append(js.Errors, err.Error())
		} else {
			for src, s := range snaps {
				d := ds(src)
				d.LatestSnapshot, d.LatestSnapshotAt = s.name, timestamppb.New(s.at)
			}
		}

		names := make([]string, 0, len(datasets))
		for n := range datasets {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			js.Datasets = append(js.Datasets, datasets[n])
		}
		sort.Strings(js.Errors)
		req.Jobs = append(req.Jobs, js)
	}
	return req
}

type snapshotInfo struct {
	name string
	at   time.Time
}

// latestSnapshots returns, per source dataset, the newest snapshot with the
// prefix under a receive dataset.
func (a *Applier) latestSnapshots(ctx context.Context, root, prefix string) (map[string]snapshotInfo, error) {
	out, err := a.Run(ctx, "zfs", "list", "-H", "-p", "-t", "snapshot", "-o", "name,creation", "-r", root)
	if err != nil {
		return nil, fmt.Errorf("list replicas under %s: %w", root, err)
	}
	latest := map[string]snapshotInfo{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			continue
		}
		dataset, snap, ok := strings.Cut(fields[0], "@")
		if !ok || !strings.HasPrefix(snap, prefix) {
			continue
		}
		src, ok := strings.CutPrefix(dataset, root+"/")
		if !ok {
			continue
		}
		sec, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		at := time.Unix(sec, 0)
		if cur, seen := latest[src]; !seen || at.After(cur.at) {
			latest[src] = snapshotInfo{name: snap, at: at}
		}
	}
	return latest, nil
}
