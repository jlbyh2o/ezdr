// Command ezdr is the EZDR client and command-line interface for Proxmox VE
// hosts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jlbyh2o/ezdr/internal/client"
	"github.com/jlbyh2o/ezdr/internal/version"
)

const usage = `Usage: ezdr <command> [options]

Commands:
  enroll <token>   Join this host to an EZDR portal
  status           Show enrollment and connection status
  unenroll         Remove EZDR's setup from this host, keeping the program
  uninstall        Remove EZDR from this host completely
  failover --plan <name>
                   Break-glass failover on the DR host when the portal is down
  run              Run the client service (used by systemd)
  boot-guard       Wait for failed-over guests to be locked (used at boot)
  version          Print version information
  help             Show this help

Run "ezdr <command> -h" for command options.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "enroll":
		err = enroll(ctx, args)
	case "status":
		err = client.Status(ctx, os.Stdout)
	case "unenroll":
		err = remove(ctx, "unenroll", args)
	case "uninstall":
		err = remove(ctx, "uninstall", args)
	case "run":
		err = client.Run(ctx)
	case "boot-guard":
		client.BootGuard(ctx, os.Stdout)
	case "failover":
		err = failoverCmd(ctx, args)
	case "version":
		fmt.Println("ezdr", version.String())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "ezdr: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		if cmd == "run" {
			slog.Error("client exited", "err", err)
		} else {
			fmt.Fprintf(os.Stderr, "\nezdr %s: %v\n", cmd, err)
		}
		os.Exit(1)
	}
}

func enroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	fs.BoolVar(yes, "y", false, "shorthand for -yes")
	force := fs.Bool("force", false, "replace an existing enrollment on this host")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: ezdr enroll [options] <token>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	return client.Enroll(ctx, client.EnrollOptions{
		Token: fs.Arg(0), Yes: *yes, Force: *force, Out: os.Stdout,
	})
}

func remove(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	force := fs.Bool("force", false, "remove EZDR even during a failover or test failover")
	// Used by the package's pre-removal script.
	pkg := fs.Bool("package-removal", false, "")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "Usage: ezdr %s [--yes] [--force]\n", cmd)
		return flag.ErrHelp
	}
	return client.Remove(ctx, client.RemoveOptions{Yes: *yes, Force: *force, Package: *pkg, Uninstall: cmd == "uninstall", Out: os.Stdout})
}

// reorder moves flags before positional arguments, so both
// "ezdr enroll --yes TOKEN" and "ezdr enroll TOKEN --yes" work.
func reorder(args []string) []string {
	var flags, rest []string
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
		} else {
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}

func failoverCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("failover", flag.ContinueOnError)
	plan := fs.String("plan", "", "the plan to fail over (name or ID)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: ezdr failover --plan <name>")
		fmt.Fprintln(fs.Output(), "\nBreak-glass failover: starts the plan's guests on this DR host from their")
		fmt.Fprintln(fs.Output(), "replicas without the portal. Use the portal instead whenever it's reachable.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *plan == "" {
		fs.Usage()
		return flag.ErrHelp
	}
	if os.Geteuid() != 0 {
		return errors.New("run it as root")
	}
	return client.BreakGlassFailover(ctx, os.Stdin, os.Stdout, *plan)
}
