// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package iec101sim_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec101sim"
	"github.com/cB-Sigma/iec60870/cs101"
)

var params = iec60870.Params{COTSize: 1, CASize: 1, IOASize: 2}

func line(t *testing.T, balanced bool) (*iec101sim.Line, *iec101sim.Station, *cs101.Link) {
	t.Helper()
	st := iec101sim.NewStation(3, 7)
	st.Set(100, iec101sim.Point{Type: iec60870.MSpNa1, Value: 0})
	st.Set(300, iec101sim.Point{Type: iec60870.MMeNc1, Value: 11.5})
	st.Controls[200] = 100
	ln := iec101sim.NewLine(balanced, st)
	if err := ln.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ln.Stop)
	c, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatal(err)
	}
	l := cs101.NewLink(cs101.NewPort(c), 1, balanced, 500*time.Millisecond, 2)
	t.Cleanup(l.Close)
	if err := l.Start(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	return ln, st, l
}

// collector decodes what the station sends and routes command answers.
type collector struct {
	mu   sync.Mutex
	cmd  iec60870.Commander
	vals map[uint32]float64
}

func (c *collector) deliver(b []byte) {
	a, err := iec60870.DecodeASDU(b, params, time.UTC)
	if err != nil {
		return
	}
	if a.Type.Control() && c.cmd.Take(a) != iec60870.Unrelated {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range a.Objects {
		c.vals[o.IOA] = o.Value
	}
}

func (c *collector) value(ioa uint32) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vals[ioa]
	return v, ok
}

// Unbalanced: the controlling station polls; interrogation answers and a
// select-before-operate command's confirmations come back as class 1
// data.
func TestSimUnbalanced(t *testing.T) {
	ln, st, l := line(t, false)
	col := &collector{vals: map[uint32]float64{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // the polling loop
		for ctx.Err() == nil {
			if l.Poll(ctx, 3, 16, col.deliver) != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	x := iec60870.Sender{Timeout: 2 * time.Second, Send: func(ctx context.Context, ty iec60870.TypeID, c iec60870.Cause, o iec60870.Object) error {
		b, err := (&iec60870.ASDU{Type: ty, Cause: c, CA: 7, Objects: []iec60870.Object{o}}).Encode(params, time.UTC)
		if err != nil {
			return err
		}
		return l.Send(ctx, 3, b)
	}}
	if err := col.cmd.Do(ctx, x, iec60870.Request{Type: iec60870.CIcNa1, Cause: iec60870.CauseActivation,
		Object: iec60870.Object{Value: iec60870.QOIStation}, Termination: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if v, ok := col.value(300); !ok || v != 11.5 {
		t.Errorf("measured value %v %v", v, ok)
	}
	if _, err := col.cmd.Operate(ctx, x, iec60870.Operation{Type: iec60870.CScNa1, Object: iec60870.Object{IOA: 200, Value: 1},
		SelectBeforeOperate: true, Termination: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	cmds := ln.Commands(st)
	if len(cmds) != 2 || !cmds[0].Select || cmds[1].Select {
		t.Errorf("commands %+v", cmds)
	}
	if gi, _, resets, polls := ln.Counts(st); gi != 1 || resets == 0 || polls == 0 {
		t.Errorf("gi %d resets %d polls %d", gi, resets, polls)
	}
}

// Balanced: the station sends its data itself as confirmed user data.
func TestSimBalanced(t *testing.T) {
	ln, st, l := line(t, true)
	if err := ln.Spontaneous(st, &iec60870.ASDU{Type: iec60870.MMeNc1, Cause: iec60870.CauseSpontaneous,
		Objects: []iec60870.Object{{IOA: 300, Value: 12.25}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-l.ASDUs():
		a, err := iec60870.DecodeASDU(b, params, time.UTC)
		if err != nil || a.Objects[0].Value != 12.25 {
			t.Errorf("received %v %v", a, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no spontaneous data on the balanced line")
	}
}
