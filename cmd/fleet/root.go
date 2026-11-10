package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"fleet/pkg/config"
	"fleet/pkg/integrations"
	"fleet/pkg/pi"
	"fleet/pkg/service"
	"fleet/pkg/tui"
)

// app holds the global flags shared by all commands.
type app struct {
	socket string
	json   bool
	piBin  string
}

func newRootCmd() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:   "fleet",
		Short: "Supervise a fleet of coding agents",
		Long: `fleet manages spaces (groups of projects), their lead agents, and the workers
and reviewers that lead agents spawn in git worktrees.

Without a subcommand it opens the cockpit. If a daemon is already listening on
$XDG_RUNTIME_DIR/fleet/fleet.sock the cockpit connects to it; otherwise it runs
the service in-process (and serves the socket while it is open).`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          a.runTUI,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&a.socket, "socket", "", "daemon socket path (default $XDG_RUNTIME_DIR/fleet/fleet.sock)")
	pf.BoolVar(&a.json, "json", false, "print machine-readable JSON")
	pf.StringVar(&a.piBin, "pi", os.Getenv("FLEET_PI_BIN"), "pi executable for agents (default \"pi\" on PATH; env FLEET_PI_BIN)")

	root.AddCommand(
		a.daemonCmd(),
		a.spaceCmd(),
		a.delegateCmd(),
		a.spawnCmd(),
		a.reportCmd(),
		a.statusCmd(),
		a.briefCmd(),
		a.reviewCmd(),
		a.mergeCmd(),
		a.closeCmd(),
		a.pingCmd(),
		a.costCmd(),
	)
	return root
}

// ---------------------------------------------------------------- plumbing

func ctxOf(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// socketPath resolves the daemon socket from --socket or the XDG runtime dir.
func (a *app) socketPath() (string, error) {
	if a.socket != "" {
		return a.socket, nil
	}
	p, err := config.NewPaths()
	if err != nil {
		return "", err
	}
	return service.SocketPath(p), nil
}

// dial connects to the running daemon.
func (a *app) dial(ctx context.Context) (*service.Client, error) {
	path, err := a.socketPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := service.Dial(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("%w; start it with `fleet daemon` or open the cockpit with `fleet`", err)
	}
	return c, nil
}

// withClient runs f with a connected client.
func (a *app) withClient(cmd *cobra.Command, f func(ctx context.Context, c *service.Client) error) error {
	ctx := ctxOf(cmd)
	c, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return f(ctx, c)
}

// emit prints v as JSON with --json, otherwise calls human.
func (a *app) emit(cmd *cobra.Command, v any, human func(w io.Writer)) error {
	w := cmd.OutOrStdout()
	if a.json {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human(w)
	return nil
}

// embedded is a service running inside this process and serving the socket.
type embedded struct {
	svc    *service.Service
	srv    *service.Server
	served chan error
}

// startService opens the fleet state and serves it on the socket.
func (a *app) startService() (*embedded, error) {
	path, err := a.socketPath()
	if err != nil {
		return nil, err
	}
	opts := service.Options{
		GitHub: integrations.NewGitHub(),
		Driver: pi.NewDriver(pi.DriverOptions{Binary: a.piBin}),
	}
	if j, err := integrations.NewJiraFromEnv(); err == nil {
		opts.Jira = j
	} else {
		slog.Debug("jira disabled", "err", err)
	}
	svc, err := service.New(opts)
	if err != nil {
		return nil, err
	}
	srv := service.NewServer(svc, path)
	if err := srv.Listen(); err != nil {
		_ = svc.Shutdown(context.Background())
		return nil, err
	}
	e := &embedded{svc: svc, srv: srv, served: make(chan error, 1)}
	go func() { e.served <- srv.Serve() }()
	return e, nil
}

// stop closes the socket and shuts the service down, waiting up to d for workers.
func (e *embedded) stop(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cerr := e.srv.Close()
	serr := e.svc.Shutdown(ctx)
	return errors.Join(cerr, serr)
}

// ---------------------------------------------------------------- cockpit

// runTUI opens the cockpit on a running daemon, or on an in-process service.
func (a *app) runTUI(cmd *cobra.Command, _ []string) error {
	ctx := ctxOf(cmd)
	var api service.API
	if c, err := a.dial(ctx); err == nil {
		defer c.Close()
		api = c
	} else {
		e, err := a.startService()
		if err != nil {
			return err
		}
		defer func() {
			if err := e.stop(10 * time.Second); err != nil {
				slog.Error("shutdown", "err", err)
			}
		}()
		api = service.Local{S: e.svc}
	}
	return tui.Run(tui.Options{Backend: service.NewAdapter(api), Context: ctx})
}
