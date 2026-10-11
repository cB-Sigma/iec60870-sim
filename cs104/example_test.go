// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package cs104_test

import (
	"context"
	"fmt"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec104sim"
	"github.com/cB-Sigma/iec60870/cs104"
)

// A controlling station against the simulated controlled station: a
// station interrogation, then a double command with select-before-operate
// that waits for its activation termination.
func Example() {
	rtu := iec104sim.New(1)
	rtu.Set(1001, iec104sim.Point{Type: iec60870.MDpNa1, Value: 2})
	rtu.Controls[2001] = 1001
	if err := rtu.Listen("", nil); err != nil {
		panic(err)
	}
	defer rtu.Stop()

	ctx := context.Background()
	link, err := cs104.Dial(ctx, rtu.Addr(), cs104.LinkConfig{})
	if err != nil {
		panic(err)
	}
	defer link.Close()
	c := cs104.NewClient(link, cs104.ClientConfig{CA: 1, OnData: func(a *iec60870.ASDU) {
		for _, o := range a.Objects {
			fmt.Println(a.Type, a.Cause, o.IOA, o.Value)
		}
	}})
	if err := c.Interrogate(ctx, iec60870.QOIStation, 5*time.Second); err != nil {
		panic(err)
	}
	_, err = c.Operate(ctx, iec60870.Operation{Type: iec60870.CDcNa1, Object: iec60870.Object{IOA: 2001, Value: 1},
		SelectBeforeOperate: true, Termination: 5 * time.Second})
	fmt.Println("open breaker:", err)
	// Output:
	// M_DP_NA_1 station interrogation 1001 2
	// M_DP_NA_1 return (remote command) 1001 1
	// open breaker: <nil>
}
