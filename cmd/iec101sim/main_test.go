// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870/cs101"
)

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

var params = iec60870.Params{COTSize: 1, CASize: 1, IOASize: 2}

// Unbalanced: a polling controlling station reads the tank level as it
// changes and runs the pump command with select-before-operate.
func TestRunnerUnbalanced(t *testing.T) {
	r, out := start(t, "-listen", "127.0.0.1:0", "-report", "50ms")
	if !strings.Contains(out.String(), "unbalanced line") {
		t.Errorf("banner %q", out)
	}
	c, err := net.Dial("tcp", r.Addr)
	if err != nil {
		t.Fatal(err)
	}
	l := cs101.NewLink(cs101.NewPort(c), 1, false, 500*time.Millisecond, 2)
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx, 3); err != nil {
		t.Fatal(err)
	}
	var cmd iec60870.Commander
	var mu sync.Mutex
	levels := map[float64]bool{}
	go func() {
		for ctx.Err() == nil {
			_ = l.Poll(ctx, 3, 16, func(b []byte) {
				a, err := iec60870.DecodeASDU(b, params, time.UTC)
				if err != nil || (a.Type.Control() && cmd.Take(a) != iec60870.Unrelated) {
					return
				}
				if a.Type == iec60870.MMeNc1 {
					mu.Lock()
					levels[a.Objects[0].Value] = true
					mu.Unlock()
				}
			})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	x := iec60870.Sender{Timeout: 2 * time.Second, Send: func(ctx context.Context, ty iec60870.TypeID, cause iec60870.Cause, o iec60870.Object) error {
		b, err := (&iec60870.ASDU{Type: ty, Cause: cause, CA: 7, Objects: []iec60870.Object{o}}).Encode(params, time.UTC)
		if err != nil {
			return err
		}
		return l.Send(ctx, 3, b)
	}}
	if _, err := cmd.Operate(ctx, x, iec60870.Operation{Type: iec60870.CScNa1, Object: iec60870.Object{IOA: 200, Value: 1},
		SelectBeforeOperate: true, Termination: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(levels)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the tank level did not change")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Balanced: the station sends its changes without being polled.
func TestRunnerBalanced(t *testing.T) {
	r, _ := start(t, "-listen", "127.0.0.1:0", "-balanced", "-report", "50ms")
	c, err := net.Dial("tcp", r.Addr)
	if err != nil {
		t.Fatal(err)
	}
	l := cs101.NewLink(cs101.NewPort(c), 1, true, 500*time.Millisecond, 2)
	defer l.Close()
	if err := l.Start(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-l.ASDUs():
		if a, err := iec60870.DecodeASDU(b, params, time.UTC); err != nil || a.Type != iec60870.MMeNc1 {
			t.Errorf("received %v %v", a, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no data on the balanced line")
	}
}

func TestRunnerFlags(t *testing.T) {
	for _, args := range [][]string{{"-link", "0"}, {"-ca", "300"}, {"extra"}} {
		if err := run(context.Background(), args, &syncBuf{}, nil); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if err := run(context.Background(), []string{"-h"}, &syncBuf{}, nil); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}
