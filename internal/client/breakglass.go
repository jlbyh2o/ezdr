package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jlbyh2o/ezdr/internal/client/failover"
	"github.com/jlbyh2o/ezdr/internal/client/guests"
	"github.com/jlbyh2o/ezdr/internal/client/zrepl"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// BreakGlassFailover runs an unplanned failover of a plan on this DR host
// without the portal (docs/design/failover.md, section 7). The operator
// confirms by typing the plan's name.
func BreakGlassFailover(ctx context.Context, in io.Reader, out io.Writer, planArg string) error {
	r := failover.NewRunner()
	recs, err := guests.LoadRecovery(r.Guests)
	if err != nil {
		return err
	}
	var rec *clientv1.PlanRecovery
	var names []string
	for _, x := range recs {
		names = append(names, x.PlanName)
		if x.PlanId == planArg || strings.EqualFold(x.PlanName, planArg) {
			rec = x
		}
	}
	if rec == nil {
		if len(names) == 0 {
			return errors.New("this host isn't the DR host of any plan")
		}
		return fmt.Errorf("no plan %q on this host; plans: %s", planArg, strings.Join(names, ", "))
	}
	stored, err := guests.Load(r.Guests, rec.PlanId)
	if err != nil {
		return err
	}
	order := slices.Clone(rec.Guests)
	slices.SortStableFunc(order, func(a, b *clientv1.RecoveryGuest) int {
		if a.StartupOrder != b.StartupOrder {
			return int(a.StartupOrder - b.StartupOrder)
		}
		return int(a.Vmid) - int(b.Vmid) //nolint:gosec // VMIDs fit
	})

	fmt.Fprintf(out, "Break-glass failover of plan %q (primary %s)\n\n", rec.PlanName, rec.PrimaryHostname)
	fmt.Fprintln(out, "This starts the plan's guests on this host from their replicas, without the portal:")
	fmt.Fprintln(out, "  - replication for the plan stops, and the replicas become the guests' disks;")
	fmt.Fprintln(out, "  - changes on the primary after the newest replicated snapshot are lost;")
	fmt.Fprintln(out, "  - the primary's copies are locked once an administrator confirms the failover in the portal.")
	fmt.Fprintln(out, "Only do this if the primary is down or unreachable.")
	fmt.Fprintln(out)
	if newest, err := r.NewestSnapshot(ctx, rec.PlanId); err != nil {
		fmt.Fprintf(out, "Newest replicated snapshot: unknown (%v)\n", err)
	} else {
		fmt.Fprintf(out, "Newest replicated snapshot: %s (%s ago)\n", newest.Local().Format(time.RFC1123), time.Since(newest).Round(time.Minute))
	}
	fmt.Fprintln(out, "Guests, in startup order:")
	for _, g := range order {
		name := "(no stored configuration)"
		if st, ok := stored[g.Vmid]; ok {
			name = st.Type
		}
		fmt.Fprintf(out, "  %d  %s\n", g.Vmid, name)
	}
	fmt.Fprintf(out, "\nType the plan's name (%s) to fail over: ", rec.PlanName)
	line, _ := bufio.NewReader(in).ReadString('\n')
	if strings.TrimSpace(line) != rec.PlanName {
		return errors.New("the name didn't match; nothing was changed")
	}

	user := os.Getenv("SUDO_USER")
	if user == "" {
		user = os.Getenv("USER")
	}
	marker := failover.Marker{PlanID: rec.PlanId, PlanName: rec.PlanName, User: user, At: time.Now().UTC()}
	// The marker comes first: from now on the client ignores the plan's jobs.
	if err := r.WriteMarker(marker); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nStopping replication for the plan…")
	if _, err := r.RemovePlanJobs(ctx, zrepl.DefaultPaths.JobsFile(), rec.PlanId); err != nil {
		return err
	}
	vmids := make([]uint32, 0, len(order))
	for _, g := range order {
		vmids = append(vmids, g.Vmid)
	}
	fmt.Fprintln(out, "Preparing the replicas and registering the guests…")
	notes, err := r.Prepare(ctx, rec.PlanId, vmids)
	for _, n := range notes {
		fmt.Fprintln(out, "  "+n)
	}
	if err != nil {
		return fmt.Errorf("%w (fix it and run the command again; it continues where it stopped)", err)
	}
	for i, g := range order {
		fmt.Fprintf(out, "Starting guest %d…\n", g.Vmid)
		if err := r.StartGuest(ctx, rec.PlanId, g.Vmid); err != nil {
			fmt.Fprintf(out, "  failed: %v\n", err)
			continue
		}
		marker.Started = append(marker.Started, g.Vmid)
		if i < len(order)-1 && g.StartupDelaySeconds > 0 {
			time.Sleep(time.Duration(g.StartupDelaySeconds) * time.Second)
		}
	}
	if err := r.WriteMarker(marker); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for _, vmid := range marker.Started {
		for {
			c, err := r.CheckGuest(ctx, rec.PlanId, vmid)
			ok := err == nil && c.Running && (!c.AgentEnabled || c.AgentOk)
			if ok || time.Now().After(deadline) {
				state := "running"
				if !ok {
					state = "not confirmed running"
				}
				fmt.Fprintf(out, "Guest %d: %s\n", vmid, state)
				break
			}
			time.Sleep(10 * time.Second)
		}
	}
	if len(rec.DnsRecords) > 0 {
		fmt.Fprintln(out, "\nThe portal can't switch DNS now. Change these records by hand (for example, in the Cloudflare dashboard):")
		for _, d := range rec.DnsRecords {
			fmt.Fprintf(out, "  %s %s -> %s\n", d.Name, d.Type, d.FailoverValue)
		}
	}
	fmt.Fprintln(out, "\nDone. When the portal is reachable again, this host reports the failover. Confirm it there to lock the")
	fmt.Fprintln(out, "primary's copies: until then, they could start on the primary too.")
	return nil
}
