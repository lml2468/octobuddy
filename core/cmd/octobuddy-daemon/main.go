// Command octobuddy-daemon is the OctoBuddy gateway daemon.
//
// It wires the full pipeline — store + router + gateway + agent driver — and
// drives it from an inbound source. Two front ends:
//
//	octobuddy-daemon # REPL on stdin (claude driver)
//	octobuddy-daemon -control /tmp/octobuddy.sock # serve the control bus (for the GUI app)
//
// With -control it listens on a Unix socket speaking the proto/ NDJSON protocol
// so the desktop app (or any client) can send commands and receive the live
// event stream. The REPL and the control bus can run together.
//
// Each inbound becomes a DM; the gateway routes it (per-session lock, rate
// limit), drives the agent, streams events to every sink (stdout + control bus),
// and persists the assistant reply + resume id for multi-turn continuity.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/clog"
	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/control/wire"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/store"
)

func main() {
	flags := parseDaemonFlags()

	// Install the slog default BEFORE any subsystem starts logging. text
	// format keeps the human-readable output operators are used to;
	// --log-json switches to structured JSON for log aggregators. --debug
	// surfaces the DEBUG-level lines (selfcheck details, lock contention
	// telemetry, etc.) that are normally too noisy for the day-to-day stream.
	clog.Setup(flags.debug, flags.logJSON, nil)

	// Config mode: load the single ~/.octobuddy/config.json and run every bot in its
	// own isolated stack. Mutually exclusive with the single-bot flag front ends.
	// `-config` with no value uses the default ~/.octobuddy/config.json. `-control`
	// additionally serves the bus so a GUI can manage all bots.
	if configFlagSet() {
		runConfigMode(flags.configPath, flags.controlSock, flags.exitParent, flags.authStdin)
		return
	}

	runSingleBotMode(flags)
}

type daemonFlags struct {
	driver      string
	agentBin    string
	fromUID     string
	dbPath      string
	maxPerMin   int
	controlSock string
	noREPL      bool
	octoAPI     string
	octoToken   string
	configPath  string
	exitParent  bool
	authStdin   bool
	debug       bool
	logJSON     bool
}

func parseDaemonFlags() daemonFlags {
	driver := flag.String("agent", "", "agent driver to spawn (claude, …; default: claude)")
	agentBin := flag.String("agent-bin", "", "agent executable (default: the driver name on PATH)")
	claudeBin := flag.String("claude-bin", "", "deprecated alias for -agent-bin")
	fromUID := flag.String("uid", "repl-user", "synthetic from_uid for REPL inbound (DM session key)")
	dbPath := flag.String("db", filepath.Join(os.TempDir(), "octobuddy-daemon.db"), "sqlite path")
	maxPerMin := flag.Int("rate", 30, "max messages per minute per session")
	controlSock := flag.String("control", "", "serve the control bus on this Unix socket path (enables GUI clients)")
	noREPL := flag.Bool("no-repl", false, "disable the stdin REPL (control-bus only)")
	octoAPI := flag.String("octo-api", "", "Octo API base URL (enables the Octo IM connector)")
	octoToken := flag.String("octo-token", "", "Octo bot token (bf_*); or set OCTOBUDDY_OCTO_TOKEN")
	configPath := flag.String("config", "", "load ~/.octobuddy/config.json (or given path) and run all configured bots")
	exitParent := flag.Bool("exit-with-parent", false, "exit when the parent process dies (set by the GUI so the daemon never outlives the app)")
	authStdin := flag.Bool("control-auth-stdin", false, "read the control-bus capability token as the first line of stdin (set by the GUI; out-of-band, never an env/argv). Off = no token: privileged bus commands are denied")
	debug := flag.Bool("debug", false, "log DEBUG-level lines (selfcheck details, gateway internals)")
	logJSON := flag.Bool("log-json", false, "emit logs as JSON (structured; for log aggregators)")
	flag.Parse()
	// -agent-bin is the canonical flag; -claude-bin is kept as a back-compat
	// alias (the canonical one wins when both are set).
	bin := *agentBin
	if bin == "" {
		bin = *claudeBin
	}
	return daemonFlags{
		driver:      *driver,
		agentBin:    bin,
		fromUID:     *fromUID,
		dbPath:      *dbPath,
		maxPerMin:   *maxPerMin,
		controlSock: *controlSock,
		noREPL:      *noREPL,
		octoAPI:     *octoAPI,
		octoToken:   *octoToken,
		configPath:  *configPath,
		exitParent:  *exitParent,
		authStdin:   *authStdin,
		debug:       *debug,
		logJSON:     *logJSON,
	}
}

func runSingleBotMode(flags daemonFlags) {
	st, err := store.Open(flags.dbPath)
	if err != nil {
		fatal("store open: %v", err)
	}
	defer st.Close()

	// Single-bot secrets are seeded from flags/env and can be updated via control.
	sec := &secretStore{}

	drv, err := agent.New(flags.driver, agent.Options{
		Bin:       flags.agentBin,
		EnvSpecFn: singleBotEnvSpecFn(sec, flags.octoAPI),
	})
	if err != nil {
		fatal("agent driver: %v", err)
	}

	started := time.Now()

	// Signal cancellation lets control-bus and IM turns finish before defers run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt := router.New(router.Config{MaxPerMinute: flags.maxPerMin})
	sinks, srv := singleBotSinks(flags.controlSock)
	connector := singleBotConnector(flags.octoAPI, flags.octoToken, sec, &sinks)
	gw := singleBotGateway(drv, st, rt, sinks, connector)

	if srv != nil {
		waitTurns, closeBus := configureSingleBotControl(ctx, srv, gw, st, drv, sec, started, flags)
		defer waitTurns()
		defer closeBus()
	}

	if connector != nil {
		startSingleBotConnector(ctx, connector, gw, flags.octoAPI)
		defer connector.WaitTurns()
	}

	fmt.Printf("octobuddy-daemon — driver=%s caps=%+v\n", drv.Name(), drv.Capabilities())
	fmt.Printf("db=%s  session=dm:%s\n", flags.dbPath, flags.fromUID)

	if flags.noREPL || connector != nil {
		waitSingleBot(ctx, stop, flags.exitParent)
		return
	}

	fmt.Println("type a message and press enter; /reset clears the session; Ctrl-D to exit")
	runREPL(context.Background(), gw, st, flags.fromUID)
}

func singleBotSinks(controlSock string) (multiSink, *control.Server) {
	sinks := multiSink{&stdoutSink{}}
	if controlSock == "" {
		return sinks, nil
	}
	srv := control.NewServer(nil)
	sinks = append(sinks, control.NewEventSink(srv))
	return sinks, srv
}

func singleBotGateway(drv agent.Driver, st *store.Store, rt *router.Router, sinks multiSink, connector *octo.Connector) *gateway.Gateway {
	gw := gateway.New(drv, st, rt, sinks)
	if connector == nil {
		return gw
	}
	return gw.WithGroupContext(groupctx.New(6000)).
		WithMediaAuth(connector.MediaAuth()).
		WithGroupBackfill(connector.BotUID, connector.BackfillFetch)
}

// singleBotEnvSpecFn supplies the driver-neutral EnvSpec for single-bot mode:
// the gateway token + octo-cli companion credential, resolved live so an
// injected token wins. The selected driver maps these onto its own var names
// (Claude → ANTHROPIC_AUTH_TOKEN). Single-bot mode has no per-bot config dir or
// gateway base URL, so those spec fields stay empty.
func singleBotEnvSpecFn(sec *secretStore, octoAPI string) func() agent.EnvSpec {
	return func() agent.EnvSpec {
		return agent.EnvSpec{
			GatewayToken: sec.GatewayToken(),
			OctoToken:    sec.OctoToken(),
			OctoAPIURL:   octoAPI,
		}
	}
}

func singleBotConnector(octoAPI, octoToken string, sec *secretStore, sinks *multiSink) *octo.Connector {
	token := octoToken
	if token == "" {
		token = os.Getenv("OCTOBUDDY_OCTO_TOKEN")
	}
	if octoAPI == "" {
		return nil
	}
	if token == "" {
		fatal("-octo-api set but no token (use -octo-token or OCTOBUDDY_OCTO_TOKEN)")
	}
	_ = sec.Set(wire.SecretKindOcto, token)
	connector := octo.NewConnector(octo.NewRESTClient(octoAPI, sec.OctoToken))
	*sinks = append(*sinks, connector)
	return connector
}

func configureSingleBotControl(ctx context.Context, srv *control.Server, gw *gateway.Gateway, st *store.Store, drv agent.Driver, sec *secretStore, started time.Time, flags daemonFlags) (func(), func()) {
	handler, target := makeCommandHandler(ctx, gw, st, drv, sec, srv, started)
	srv.SetHandler(handler)
	configureBusAuth(srv, flags.authStdin)
	return target.turnsWG.Wait, serveControlBus(srv, flags.controlSock)
}

func startSingleBotConnector(ctx context.Context, connector *octo.Connector, gw *gateway.Gateway, octoAPI string) {
	connector.SetGateway(gw)
	go func() {
		if err := connector.Run(ctx); err != nil {
			clog.For("octo").Error("connector", "err", err)
		}
	}()
	fmt.Printf("octo connector started (api=%s)\n", octoAPI)
}

func waitSingleBot(ctx context.Context, stop context.CancelFunc, exitParent bool) {
	if exitParent {
		watchParentExit(func() {
			fmt.Fprintln(os.Stderr, "parent exited; shutting down")
			stop()
		})
	}
	fmt.Println("running (control bus / IM connector); press Ctrl-C to exit")
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "shutting down")
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
