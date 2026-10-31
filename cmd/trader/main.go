package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const usage = `trader — the Candylario engine

  trader radar          watch every Hyperliquid perpetual and print unusual
                        moves as they happen; --json, --interval 30s,
                        --testnet, --no-warmup; no keys needed
  trader serve          run the radar and the engine API (what Docker runs);
                        needs DATABASE_URL and ENGINE_MASTER_KEY, and
                        ENGINE_API_TOKEN for the API
  trader vault rewrap   seal every stored key under the current master key,
                        after a rotation (see the README)`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, env{}); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// env holds what tests replace: the HTTP client and the clock.
type env struct {
	HTTP *http.Client
	Now  func() time.Time
}

// commit is the source commit this binary was built from, set by the
// Dockerfile with -ldflags "-X main.commit=...".
var commit = "dev"

func run(ctx context.Context, args []string, out io.Writer, e env) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(out, usage)
		return nil
	}
	if err := loadEnv(".env"); err != nil {
		return err
	}
	if e.HTTP == nil {
		e.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if args[0] == "radar" {
		return runRadar(ctx, args[1:], out, e)
	}
	cmd := strings.Join(args, " ")
	if cmd != "serve" && cmd != "vault rewrap" {
		return fmt.Errorf("unknown command %q; run `trader help`", cmd)
	}
	svc, closeAll, err := openFleet(ctx, e)
	if err != nil {
		return err
	}
	defer closeAll()
	if cmd == "serve" {
		return serve(ctx, svc, e, out)
	}
	n, err := svc.Rewrap(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "re-wrapped %d keys under master key version %d\n", n, svc.Vault.Version())
	return nil
}
