// Command sync2connect syncs health data between providers, e.g. Withings
// body measurements into Garmin Connect.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/matthieudolci/sync2connect/internal/config"
	"github.com/matthieudolci/sync2connect/internal/provider"
	"github.com/matthieudolci/sync2connect/internal/state"
	"github.com/matthieudolci/sync2connect/internal/syncer"

	// Providers register themselves with the provider registry.
	_ "github.com/matthieudolci/sync2connect/internal/provider/garmin"
	_ "github.com/matthieudolci/sync2connect/internal/provider/withings"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

var usage = `sync2connect - sync health data between providers

Usage:
  sync2connect [-config path] auth <provider> [--manual]
  sync2connect [-config path] sync [--once]
  sync2connect version

Commands:
  auth      Run the interactive authentication flow for a provider
            (--manual prints URLs and reads codes from stdin instead of
            starting a local callback server; useful on headless machines).
  sync      Sync all configured routes. Runs continuously when
            sync.interval is set in the config, unless --once is given.
  version   Print the version.

The config file is located via -config, the SYNC2CONNECT_CONFIG environment
variable, or ./config.yaml, in that order.

Registered providers: ` + strings.Join(provider.Names(), ", ") + "\n"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	flags := flag.NewFlagSet("sync2connect", flag.ExitOnError)
	configPath := flags.String("config", "", "path to the configuration file")
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := flags.Args()
	if len(args) == 0 {
		flags.Usage()
		return errors.New("missing command")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "auth":
		return runAuth(ctx, *configPath, args[1:])
	case "sync":
		return runSync(ctx, log, *configPath, args[1:])
	case "help":
		flags.Usage()
		return nil
	default:
		flags.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// resolveConfigPath picks the config file from the flag, environment or the
// default location.
func resolveConfigPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("SYNC2CONNECT_CONFIG"); env != "" {
		return env
	}
	return "config.yaml"
}

func runAuth(ctx context.Context, configPath string, args []string) error {
	flags := flag.NewFlagSet("auth", flag.ExitOnError)
	manual := flags.Bool("manual", false, "copy/paste authentication without a local callback server")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: sync2connect auth <provider> [--manual] (available: %s)", strings.Join(provider.Names(), ", "))
	}
	name := flags.Arg(0)

	cfg, err := config.Load(resolveConfigPath(configPath))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}

	prov, err := provider.New(name, provider.Config{
		Settings:   cfg.ProviderSettings(name),
		StateDir:   cfg.StateDir,
		ManualAuth: *manual,
	})
	if err != nil {
		return err
	}
	auth, ok := prov.(provider.Authenticator)
	if !ok {
		return fmt.Errorf("provider %q does not require authentication", name)
	}
	return auth.Authenticate(ctx, promptStdin)
}

func runSync(ctx context.Context, log *slog.Logger, configPath string, args []string) error {
	flags := flag.NewFlagSet("sync", flag.ExitOnError)
	once := flags.Bool("once", false, "run a single sync pass even when an interval is configured")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(resolveConfigPath(configPath))
	if err != nil {
		return err
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		return err
	}

	// Providers are instantiated once and shared across routes.
	instances := map[string]provider.Provider{}
	instance := func(name string) (provider.Provider, error) {
		if p, ok := instances[name]; ok {
			return p, nil
		}
		p, err := provider.New(name, provider.Config{
			Settings: cfg.ProviderSettings(name),
			StateDir: cfg.StateDir,
		})
		if err != nil {
			return nil, err
		}
		instances[name] = p
		return p, nil
	}

	var routes []*syncer.Route
	for _, rc := range cfg.Sync.Routes {
		srcProv, err := instance(rc.Source)
		if err != nil {
			return err
		}
		src, ok := srcProv.(provider.Source)
		if !ok {
			return fmt.Errorf("provider %q cannot be used as a source", rc.Source)
		}
		dstProv, err := instance(rc.Destination)
		if err != nil {
			return err
		}
		dst, ok := dstProv.(provider.Destination)
		if !ok {
			return fmt.Errorf("provider %q cannot be used as a destination", rc.Destination)
		}
		routes = append(routes, &syncer.Route{
			Source:          src,
			Destination:     dst,
			State:           store,
			InitialLookback: time.Duration(cfg.Sync.InitialLookback),
			Log:             log,
		})
	}

	interval := time.Duration(cfg.Sync.Interval)
	if interval > 0 && !*once {
		log.Info("starting sync loop", "interval", interval.String(), "routes", len(routes), "version", version)
	}

	for {
		failures := 0
		for _, route := range routes {
			if err := route.Run(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failures++
				log.Error("sync failed", "route", route.Key(), "error", err)
			}
		}
		if interval == 0 || *once {
			if failures > 0 {
				return fmt.Errorf("%d route(s) failed", failures)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		case <-time.After(interval):
		}
	}
}

// promptStdin reads a value from the terminal, hiding the input for secrets
// when stdin is a TTY.
func promptStdin(label string, secret bool) (string, error) {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	fd := int(os.Stdin.Fd())
	if secret && term.IsTerminal(fd) {
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(raw), err
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
