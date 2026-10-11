// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Command iec101sim serves a simulated IEC 60870-5-101 line with one
// controlled station, reached over TCP the way a transparent serial-to-IP
// converter is (FT1.2 frames on the byte stream), so a controlling station
// can be tried without a device or a serial port. The line is unbalanced
// (the station answers polls) or, with -balanced, balanced (the station
// also sends its data as confirmed user data). The station has link
// address -link (default 3) and common address -ca (default 7), with
// IEC 101's usual field sizes (cause 1, common address 1, object address
// 2 octets) and a small demo database:
//
//	100  M_SP_NA_1  pump running        (changed by 200)
//	200  C_SC_NA_1  pump command        (select-before-operate)
//	300  M_ME_NC_1  tank level, m       (moves every -report, spontaneous)
//
// For example:
//
//	iec101sim -listen 127.0.0.1:2410
//	iec101sim -listen 127.0.0.1:2410 -balanced
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec101sim"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "iec101sim:", err)
		os.Exit(1)
	}
}

// running is what run started.
type running struct {
	Addr    string
	Line    *iec101sim.Line
	Station *iec101sim.Station
}

// run serves until ctx ends. ready, when not nil, is called once the line
// listens.
func run(ctx context.Context, args []string, out io.Writer, ready func(running)) error {
	fs := flag.NewFlagSet("iec101sim", flag.ContinueOnError)
	fs.SetOutput(out)
	listen := fs.String("listen", "127.0.0.1:2410", "address to listen on (one controlling station at a time)")
	balanced := fs.Bool("balanced", false, "balanced transmission instead of unbalanced polling")
	link := fs.Uint("link", 3, "link address of the station (1 to 254)")
	ca := fs.Uint("ca", 7, "common address of the station (1 to 254)")
	report := fs.Duration("report", 5*time.Second, "how often the tank level changes (0: never)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *link < 1 || *link > 254 || *ca < 1 || *ca > 254 {
		return errors.New("-link and -ca are 1 to 254")
	}
	st := iec101sim.NewStation(uint16(*link), uint16(*ca))
	st.Set(100, iec101sim.Point{Type: iec60870.MSpNa1, Value: 0})
	st.Set(300, iec101sim.Point{Type: iec60870.MMeNc1, Value: 3.5})
	st.Controls[200] = 100
	ln := iec101sim.NewLine(*balanced, st)
	if err := ln.ListenAddr(*listen); err != nil {
		return err
	}
	defer ln.Stop()
	mode := "unbalanced"
	if *balanced {
		mode = "balanced"
	}
	fmt.Fprintf(out, "iec101sim: %s line, station at link address %d, common address %d, at %s\n", mode, *link, *ca, ln.Addr())
	if ready != nil {
		ready(running{Addr: ln.Addr(), Line: ln, Station: st})
	}
	var tick <-chan time.Time
	if *report > 0 {
		t := time.NewTicker(*report)
		defer t.Stop()
		tick = t.C
	}
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			v := 3.5 + float64(n%4)*0.25
			ln.Lock()
			st.Set(300, iec101sim.Point{Type: iec60870.MMeNc1, Value: v})
			ln.Unlock()
			_ = ln.Spontaneous(st, &iec60870.ASDU{Type: iec60870.MMeNc1, Cause: iec60870.CauseSpontaneous,
				Objects: []iec60870.Object{{IOA: 300, Value: v}}})
		}
	}
}
