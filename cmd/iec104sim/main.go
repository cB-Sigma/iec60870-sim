// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Command iec104sim serves a simulated IEC 60870-5-104 controlled station,
// so a controlling station (SCADA, gateway) can be tried without a device.
// It listens on TCP (TLS with -cert and -key) and holds a small demo
// database at common address -ca (default 1):
//
//	1001  M_DP_NA_1  breaker position      (2 closed, 1 open)
//	2001  C_DC_NA_1  breaker command       (select-before-operate; changes 1001)
//	3001  M_ME_NC_1  feeder voltage, V     (moves every -report, spontaneous)
//	3002  M_ME_NC_1  feeder current, A
//	4001  M_IT_NA_1  energy counter        (counter interrogation)
//
// For example:
//
//	iec104sim -listen 127.0.0.1:2404
//	iec104sim -listen 127.0.0.1:19998 -cert rtu.pem -key rtu.key
//	iec104sim -listen 127.0.0.1:2404 -secure -trust 04ab...   (IEC 62351-5 A-Profile)
//
// With -secure the station runs the IEC TS 60870-5-7 A-Profile with a
// fresh self-signed secp256r1 identity whose public key it prints; -trust
// pins the controlling station's public key (hex, as printed by the
// same option of another run, or sec.Identity.Key.PublicKey). All keys are
// demo values.
package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec104sim"
	"github.com/cB-Sigma/iec60870/cs104"
	"github.com/cB-Sigma/iec60870/sec"
	"github.com/cB-Sigma/iotsec"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "iec104sim:", err)
		os.Exit(1)
	}
}

// running is what run started.
type running struct {
	Addr      string
	Station   *iec104sim.Station
	PublicKey []byte // with -secure: the station's public key
}

// run serves until ctx ends. ready, when not nil, is called once the
// station listens.
func run(ctx context.Context, args []string, out io.Writer, ready func(running)) error {
	fs := flag.NewFlagSet("iec104sim", flag.ContinueOnError)
	fs.SetOutput(out)
	listen := fs.String("listen", "127.0.0.1:2404", "address to listen on")
	ca := fs.Uint("ca", 1, "common address of the station (1 to 65534)")
	cert := fs.String("cert", "", "serve under TLS with this certificate (PEM file; needs -key)")
	key := fs.String("key", "", "private key of -cert (PEM file)")
	secure := fs.Bool("secure", false, "run the IEC 62351-5 A-Profile with a fresh self-signed identity")
	trust := fs.String("trust", "", "with -secure: the controlling station's public key (hex)")
	report := fs.Duration("report", 5*time.Second, "how often the demo values change (0: never)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *ca < 1 || *ca > 65534 {
		return errors.New("-ca is 1 to 65534")
	}
	s := iec104sim.New(uint16(*ca))
	if *cert != "" || *key != "" {
		c, err := tls.LoadX509KeyPair(*cert, *key)
		if err != nil {
			return fmt.Errorf("-cert/-key: %w", err)
		}
		s.TLS = iotsec.StdTLS{Config: &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}}
	}
	var pub []byte
	if *secure {
		if *trust == "" {
			return errors.New("-secure needs -trust (the controlling station's public key)")
		}
		peer, err := hex.DecodeString(*trust)
		if err != nil {
			return fmt.Errorf("-trust: %w", err)
		}
		id, err := sec.NewIdentity(sec.Secp256r1, "iec104sim", 365*24*time.Hour)
		if err != nil {
			return err
		}
		pub = id.Key.PublicKey()
		s.Security, err = sec.SecureConfig(sec.Config{Link: sec.TCP, Params: sec.Params104, MaxASDU: cs104.MaxASDU, CA: uint16(*ca),
			AIS: 1, Key: id.Key, Certificate: id.Certificate, Trust: sec.Trust{PublicKeys: [][]byte{peer}}}, sec.NewEndpoint)
		if err != nil {
			return err
		}
	} else if *trust != "" {
		return errors.New("-trust needs -secure")
	}
	s.Set(1001, iec104sim.Point{Type: iec60870.MDpNa1, Value: 2})
	s.Set(3001, iec104sim.Point{Type: iec60870.MMeNc1, Value: 230.5})
	s.Set(3002, iec104sim.Point{Type: iec60870.MMeNc1, Value: 12.25})
	s.Set(4001, iec104sim.Point{Type: iec60870.MItNa1, Value: 0})
	s.Controls[2001] = 1001
	if err := s.Listen(*listen, nil); err != nil {
		return err
	}
	defer s.Stop()
	fmt.Fprintf(out, "iec104sim: controlled station, common address %d, at %s\n", *ca, s.Addr())
	if pub != nil {
		fmt.Fprintf(out, "iec104sim: A-Profile public key %x\n", pub)
	}
	if ready != nil {
		ready(running{Addr: s.Addr(), Station: s, PublicKey: pub})
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
			v := 230.5 + float64(n%5) - 2
			s.Set(3001, iec104sim.Point{Type: iec60870.MMeNc1, Value: v})
			s.Set(4001, iec104sim.Point{Type: iec60870.MItNa1, Value: float64(n * 10)})
			_ = s.Send(&iec60870.ASDU{Type: iec60870.MMeNc1, Cause: iec60870.CauseSpontaneous,
				Objects: []iec60870.Object{{IOA: 3001, Value: v}}})
		}
	}
}
