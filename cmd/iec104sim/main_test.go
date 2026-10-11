// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870/cs104"
	"github.com/cB-Sigma/iec60870/sec"
)

// syncBuf is a goroutine-safe output buffer.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func start(t *testing.T, args ...string) (running, *syncBuf) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	got := make(chan running, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, out, func(r running) { got <- r }) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	select {
	case r := <-got:
		return r, out
	case err := <-done:
		t.Fatalf("run ended: %v (%s)", err, out)
	case <-time.After(5 * time.Second):
		t.Fatal("the simulator did not start")
	}
	return running{}, nil
}

type values struct {
	mu sync.Mutex
	m  map[uint32]float64
}

func (v *values) on(a *iec60870.ASDU) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, o := range a.Objects {
		v.m[o.IOA] = o.Value
	}
}

func (v *values) get(ioa uint32) (float64, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	x, ok := v.m[ioa]
	return x, ok
}

// The demo database answers an interrogation, the breaker command
// changes the breaker position, and the voltage moves spontaneously.
func TestRunnerDemo(t *testing.T) {
	r, out := start(t, "-listen", "127.0.0.1:0", "-report", "50ms")
	if !strings.Contains(out.String(), "common address 1") {
		t.Errorf("banner %q", out)
	}
	l, err := cs104.Dial(context.Background(), r.Addr, cs104.LinkConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	v := &values{m: map[uint32]float64{}}
	c := cs104.NewClient(l, cs104.ClientConfig{CA: 1, Timeout: 2 * time.Second, OnData: v.on})
	ctx := context.Background()
	if err := c.Interrogate(ctx, iec60870.QOIStation, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if b, ok := v.get(1001); !ok || b != 2 {
		t.Errorf("breaker %v %v", b, ok)
	}
	if _, err := c.Operate(ctx, iec60870.Operation{Type: iec60870.CDcNa1, Object: iec60870.Object{IOA: 2001, Value: 1},
		SelectBeforeOperate: true, Termination: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if b, _ := v.get(1001); b != 1 {
		t.Errorf("breaker %v after opening", b)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if x, _ := v.get(3001); x != 0 && x != float64(float32(230.5)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no spontaneous voltage change")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// -secure runs the A-Profile: a controlling station that pins the
// printed key associates and reads data as Secure Data.
func TestRunnerSecure(t *testing.T) {
	scada, err := sec.NewIdentity(sec.Secp256r1, "scada", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r, out := start(t, "-listen", "127.0.0.1:0", "-report", "0", "-secure", "-trust", hex.EncodeToString(scada.Key.PublicKey()))
	if !strings.Contains(out.String(), hex.EncodeToString(r.PublicKey)) {
		t.Errorf("the public key is not printed: %q", out)
	}
	sl, err := sec.SecureConfig(sec.Config{Controlling: true, Link: sec.TCP, Params: sec.Params104, MaxASDU: cs104.MaxASDU, CA: 1,
		AIM: 1, Key: scada.Key, Certificate: scada.Certificate, Trust: sec.Trust{PublicKeys: [][]byte{r.PublicKey}}}, sec.NewEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sl.Run(ctx)
	l, err := cs104.Dial(ctx, r.Addr, cs104.LinkConfig{Security: sl})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	v := &values{m: map[uint32]float64{}}
	c := cs104.NewClient(l, cs104.ClientConfig{CA: 1, Timeout: 3 * time.Second, OnData: v.on})
	deadline := time.Now().Add(5 * time.Second)
	for !sl.Endpoint().Status().SessionKeysValid {
		if time.Now().After(deadline) {
			t.Fatal("no session keys")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := c.Interrogate(ctx, iec60870.QOIStation, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.get(3002); !ok {
		t.Error("no data under the A-Profile")
	}
}

func TestRunnerFlags(t *testing.T) {
	for _, args := range [][]string{{"-ca", "0"}, {"-secure"}, {"-trust", "00"}, {"-cert", "missing.pem", "-key", "missing.key"}, {"extra"}} {
		if err := run(context.Background(), args, &syncBuf{}, nil); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if err := run(context.Background(), []string{"-h"}, &syncBuf{}, nil); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}
