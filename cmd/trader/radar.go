package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/candylario/trader/internal/hyperliquid"
	"github.com/candylario/trader/internal/radar"
)

// runRadar watches every perpetual and prints each unusual move as it
// happens. It needs no keys.
func runRadar(ctx context.Context, args []string, out io.Writer, e env) error {
	fs := flag.NewFlagSet("radar", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	interval := fs.Duration("interval", 30*time.Second, "time between samples")
	asJSON := fs.Bool("json", false, "print each alert as a JSON line")
	testnet := fs.Bool("testnet", false, "watch the testnet instead of the live exchange")
	noWarm := fs.Bool("no-warmup", false, "skip loading the past day of candles")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(out, usage)
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments; run `trader help`")
	}
	url := hyperliquid.MainnetURL
	if *testnet {
		url = hyperliquid.TestnetURL
	}
	client := &hyperliquid.Client{HTTP: e.HTTP, BaseURL: url, Now: e.Now}
	enc := json.NewEncoder(out)
	w := &radar.Watcher{
		Source:   client,
		Interval: *interval,
		Now:      e.Now,
		Logf:     func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) },
		Alert: func(a radar.Alert) {
			if *asJSON {
				enc.Encode(a)
				return
			}
			fmt.Fprintf(out, "%s  %s\n", a.At.UTC().Format("15:04"), a)
		},
	}
	fmt.Fprintf(out, "radar on %s, a sample every %s\n", url, *interval)
	if !*noWarm {
		if err := w.Warm(ctx, client); err != nil {
			return quiet(err)
		}
	}
	return quiet(w.Run(ctx))
}

// quiet turns the end of a run by Ctrl-C into a clean exit.
func quiet(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
