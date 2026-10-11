// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package cs101_test

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec101sim"
	"github.com/cB-Sigma/iec60870/cs101"
)

// An unbalanced line through a serial-to-IP converter (here the
// simulator): start the station's link, ask for a station interrogation,
// and poll until the answers are in.
func ExampleNewLink() {
	st := iec101sim.NewStation(3, 7)
	st.Set(300, iec101sim.Point{Type: iec60870.MMeNc1, Value: 3.5})
	line := iec101sim.NewLine(false, st)
	if err := line.Listen(); err != nil {
		panic(err)
	}
	defer line.Stop()

	conn, err := net.Dial("tcp", line.Addr())
	if err != nil {
		panic(err)
	}
	link := cs101.NewLink(cs101.NewPort(conn), 1, false, 500*time.Millisecond, 2)
	defer link.Close()
	ctx := context.Background()
	if err := link.Start(ctx, 3); err != nil {
		panic(err)
	}
	p := iec60870.Params{COTSize: 1, CASize: 1, IOASize: 2}
	gi, _ := (&iec60870.ASDU{Type: iec60870.CIcNa1, Cause: iec60870.CauseActivation, CA: 7,
		Objects: []iec60870.Object{{Value: iec60870.QOIStation}}}).Encode(p, time.UTC)
	if err := link.Send(ctx, 3, gi); err != nil {
		panic(err)
	}
	done := false
	for !done {
		_ = link.Poll(ctx, 3, 16, func(b []byte) {
			a, err := iec60870.DecodeASDU(b, p, time.UTC)
			if err != nil {
				return
			}
			switch {
			case a.Type == iec60870.MMeNc1:
				fmt.Println(a.Type, a.Objects[0].IOA, a.Objects[0].Value)
			case a.Type == iec60870.CIcNa1 && a.Cause == iec60870.CauseActTerm:
				done = true
			}
		})
	}
	// Output: M_ME_NC_1 300 3.5
}
