package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func (a *app) daemonCmd() *cobra.Command {
	var grace time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the headless fleet service on the Unix socket",
		Long: `Run the fleet service without a UI. It listens on --socket (default
$XDG_RUNTIME_DIR/fleet/fleet.sock) until interrupted, then stops workers and
flushes the spend ledger. Workers that were running resume with ` + "`fleet spawn`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.startService()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "fleet daemon listening on", e.srv.Path())
			select {
			case <-ctxOf(cmd).Done():
			case err := <-e.served:
				if err != nil {
					_ = e.stop(grace)
					return err
				}
			}
			return e.stop(grace)
		},
	}
	cmd.Flags().DurationVar(&grace, "grace", 30*time.Second, "how long to wait for workers on shutdown")
	return cmd
}
