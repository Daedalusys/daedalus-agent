// Command daedalus-agent runs on each Daedalus fleet node. It accepts
// mTLS gRPC commands from the control plane: Register, ApplyIntent,
// ReportStatus, and friends. The agent is the entry-ticket product
// for the P0.5 fleet control plane; it is superseded by daedalus-atomic
// once per-node migration completes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/daemon"
	"github.com/Daedalusys/daedalus-agent/internal/rpc"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
)

// version is overridden at build time via -ldflags "-X main.version=…".
// Default value is the in-tree development version.
var version = "0.1.0-dev"

// ShutdownTimeout bounds the signal-triggered graceful shutdown. It is
// larger than daemon.ShutdownTimeout so signal handling has headroom
// over the daemon's own internal drain window.
const ShutdownTimeout = 35 * time.Second

func main() {
	if err := newRootCmd().Execute(); err != nil {
		log.SetOutput(os.Stderr)
		log.Print(err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var cfgPath string

	root := &cobra.Command{
		Use:   "daedalus-agent",
		Short: "Daedalus fleet agent — runs on each node, accepts control-plane RPCs",
		Long: `daedalus-agent is the entry-ticket agent for the Daedalus fleet control plane.
It registers with the control plane, applies intents, and reports status
over mTLS gRPC. Once the fleet transitions to daedalus-atomic, the agent
is retired in favor of the atomic-aware runtime.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&cfgPath, "config", "",
		"path to YAML config file (default: built-in defaults)")

	root.AddCommand(newDaemonCmd(&cfgPath))
	root.AddCommand(newVersionCmd())
	root.AddCommand(newRegisterCmd(&cfgPath))
	root.AddCommand(newApplyCmd(&cfgPath))
	root.AddCommand(newReportCmd(&cfgPath))

	return root
}

// loadConfig reads and validates the config. On error it wraps the
// underlying message with a clear hint about which flag is responsible.
//
// Loader precedence (T2): env vars (DAEDALUS_AGENT_*) beat YAML beat
// built-in defaults. This lets ops override a single field without
// editing /etc/daedalus-agent/config.yaml — see internal/config
// loader.go for the exact mapping.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.LoadWithEnvOverride(path)
	if err != nil {
		return nil, fmt.Errorf("--config=%q: %w", path, err)
	}
	// Resolve relative paths against the config file's directory so
	// that operators can ship a single config file with relative
	// cert/data paths. An empty path falls back to current dir.
	if path != "" {
		base := filepath.Dir(path)
		if base == "" || base == "." {
			base, _ = os.Getwd()
		}
		cfg.ResolveDirs(base)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// --- daemon subcommand tree -------------------------------------------------
//
// `daemon` with no subcommand defaults to `start` (per the brief:
// "start (后台跑, 默认)"). Both the explicit `daemon start` and the
// bare `daemon` invocation route through runDaemonStart so the body
// is single-sourced.
//
// Bare `daedalus-agent` (no subcommand) does NOT auto-start the
// daemon: it prints help. Auto-starting a fleet agent on bare
// invocation is a production-surprise hazard ("I just typed --help
// and now my agent is running"), so the operator must opt in to
// `daemon` explicitly.

func newDaemonCmd(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the long-lived daemon (default subcommand: start)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonStart(cmd, *cfgPath)
		},
		SilenceUsage: true,
	}
	cmd.AddCommand(newDaemonStartCmd(cfgPath))
	cmd.AddCommand(newDaemonStatusCmd(cfgPath))
	return cmd
}

func newDaemonStartCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon (default subcommand under daemon)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonStart(cmd, *cfgPath)
		},
	}
}

// runDaemonStart holds the body of the `daemon start` subcommand.
// It is invoked by both newDaemonStartCmd (when the user types
// `daemon start` explicitly) and newDaemonCmd (when the user types
// `daemon` with no subcommand, per the brief's "default" clause).
// Single-sourcing the body prevents the two paths from drifting.
func runDaemonStart(cmd *cobra.Command, cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM cancel the parent context; the daemon then has
	// up to ShutdownTimeout to drain.
	ctx, stop := signal.NotifyContext(cmd.Context(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := daemon.New(cfg)

	// T3 wire-in: bring up the mTLS gRPC server before the daemon's
	// run loop blocks. The shutdown hook drains in-flight RPCs via
	// GracefulStop on SIGTERM (still bounded by daemon.ShutdownTimeout).
	rpcSrv, err := startRPCServer(cfg)
	if err != nil {
		return fmt.Errorf("rpc: %w", err)
	}
	if rpcSrv != nil {
		if err := d.RegisterShutdownHook(func(shutdownCtx context.Context) error {
			// We do NOT block on GracefulStop: it respects
			// shutdownCtx and signals existing RPCs to drain.
			// If shutdownCtx expires mid-drain, we still return
			// nil — the daemon's deadline is handled by the
			// supervisor (systemd TimeoutStopSec=35s).
			doneCh := make(chan struct{})
			go func() {
				rpcSrv.GracefulStop()
				close(doneCh)
			}()
			select {
			case <-doneCh:
				return nil
			case <-shutdownCtx.Done():
				// Force-stop rather than wait: prevents a
				// pathological client from holding the
				// drain open past the deadline.
				rpcSrv.Stop()
				return nil
			}
		}); err != nil {
			rpcSrv.Stop()
			return fmt.Errorf("rpc: register shutdown hook: %w", err)
		}
	}

	log.Printf("daedalus-agent %s starting (agent_id=%s, control_plane=%s)",
		version, cfg.AgentID, cfg.ControlPlaneAddr)

	if err := d.Start(ctx); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	log.Printf("daedalus-agent stopped")
	return nil
}

// startRPCServer brings up the mTLS gRPC server using the cert
// material managed by internal/cert. Returns (nil, nil) if cfg's
// cert paths are unset (e.g. tests) so the wire-in is opt-in by
// configuration rather than always-on.
//
// When the cert loader returns ErrUnparseableCert (because the
// disk holds a half-written or zero-byte file), this surfaces as
// a startup error — better to fail fast than to serve an
// untrusted handshake to the control plane.
func startRPCServer(cfg *config.Config) (*rpc.Server, error) {
	// Skip RPC bring-up when cert paths are unset. Production
	// deployments always set these (T2's postinstall writes them
	// under /var/lib/daedalus-agent/cert/); tests opt out.
	if cfg.CAPath == "" || cfg.CertPath == "" ||
		cfg.KeyPath == "" || cfg.CAKeyPath == "" {
		log.Printf("rpc: cert paths unset; mTLS gRPC server disabled (development mode)")
		return nil, nil
	}
	loader, err := cert.New(cert.Config{
		CAPath:     cfg.CAPath,
		CAKeyPath:  cfg.CAKeyPath,
		CertPath:   cfg.CertPath,
		KeyPath:    cfg.KeyPath,
		CommonName: cfg.AgentID,
	})
	if err != nil {
		return nil, err
	}
	tlsCfg, err := loader.Load()
	if err != nil {
		return nil, err
	}
	info, err := cert.LoaderInfo(loader)
	if err != nil {
		return nil, fmt.Errorf("cert info: %w", err)
	}
	rpcSrv, err := rpc.NewServer(rpc.DefaultListenAddr, tlsCfg, rpc.ServiceDeps{
		Cfg:      cfg,
		CertInfo: info,
		Snapshot: snapshot.NewDetector(),
	})
	if err != nil {
		return nil, err
	}
	ln, err := rpcSrv.Listener()
	if err != nil {
		return nil, fmt.Errorf("rpc listen %s: %w", rpc.DefaultListenAddr, err)
	}
	go func() {
		log.Printf("rpc: serving mTLS on %s", ln.Addr())
		if err := rpcSrv.Serve(ln); err != nil {
			log.Printf("rpc: serve stopped: %v", err)
		}
	}()
	return rpcSrv, nil
}

func newDaemonStatusCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print daemon status",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*cfgPath)
			if err != nil {
				return err
			}
			fmt.Printf("daedalus-agent %s\n", version)
			fmt.Printf("  agent_id:           %s\n", cfg.AgentID)
			fmt.Printf("  control_plane_addr: %s\n", cfg.ControlPlaneAddr)
			fmt.Printf("  data_dir:           %s\n", cfg.DataDir)
			fmt.Printf("  audit_dir:          %s\n", cfg.AuditDir)
			// T1 skeleton has no supervisor query path yet; the
			// local process state is the closest proxy. Once T3
			// wires the gRPC server, status will consult it.
			fmt.Printf("  local_state:        %s\n", daemon.StateRunning)
			return nil
		},
	}
}

// --- version subcommand -----------------------------------------------------

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and exit",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf("daedalus-agent %s\n", version)
		},
	}
}

// --- RPC client stubs -------------------------------------------------------
//
// These three subcommands exist to exercise the gRPC client wiring in T3
// and the RPC handler paths in T5. For T1 they print a clear
// not-implemented message and exit non-zero, after loading the config so
// the wiring is exercised end-to-end.

func newRegisterCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "register",
		Short: "Manually invoke Register against the control plane",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*cfgPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr,
				"register: not yet implemented (T5 will add this RPC); would dial %s as %s\n",
				cfg.ControlPlaneAddr, cfg.AgentID)
			return errNotImplemented("register", "T5")
		},
	}
}

func newApplyCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Manually invoke ApplyIntent against the control plane",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*cfgPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr,
				"apply: not yet implemented (T5 will add this RPC); would dial %s as %s\n",
				cfg.ControlPlaneAddr, cfg.AgentID)
			return errNotImplemented("apply", "T5")
		},
	}
}

func newReportCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Manually invoke ReportStatus against the control plane",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*cfgPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr,
				"report: not yet implemented (T5 will add this RPC); would dial %s as %s\n",
				cfg.ControlPlaneAddr, cfg.AgentID)
			return errNotImplemented("report", "T5")
		},
	}
}

// errNotImplemented returns a sentinel-shaped error that callers can
// errors.Is against once a real implementation lands. The task name
// tells the operator which task will close the gap.
func errNotImplemented(cmd, task string) error {
	return errors.New(cmd + ": not implemented (planned for " + task + ")")
}
