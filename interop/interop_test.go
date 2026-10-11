// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Package interop runs the simulated controlled stations of this module
// against lib60870-C v2.4.1 controlling stations
// (github.com/mz-automation/lib60870, GPLv3, used only as a test peer in a
// container built from ./peer). The tests are skipped unless
// SIGMA_IEC60870_INTEROP=1 is set and Docker is running.
package interop

import (
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec101sim"
	"github.com/cB-Sigma/iec60870-sim/sim/iec104sim"
)

func port(t *testing.T, addr string) string {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// station104 is a simulated IEC 104 station with a few points and
// controls.
func station104(t *testing.T, cfg *tls.Config) *iec104sim.Station {
	t.Helper()
	s := iec104sim.New(1)
	s.Set(100, iec104sim.Point{Type: iec60870.MSpNa1, Value: 1})
	s.Set(101, iec104sim.Point{Type: iec60870.MDpNa1, Value: 1})
	s.Set(200, iec104sim.Point{Type: iec60870.MMeNc1, Value: 230.5})
	s.Controls[5000] = 100
	s.Controls[5001] = 101
	s.Controls[5002] = 200
	if err := s.Listen("127.0.0.1:0", cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s
}

// run104 drives a lib60870 client through the station's procedures.
func run104(t *testing.T, s *iec104sim.Station, p *peer) {
	p.expect(60*time.Second, "READY")
	p.send("startdt")
	p.expect(10*time.Second, "EVENT 2") // CS104_CONNECTION_STARTDT_CON_RECEIVED

	t.Run("general interrogation", func(t *testing.T) {
		p.send("gi")
		p.expect(5*time.Second, "type=100", "cot=7")
		p.expect(5*time.Second, "type=1 ", "cot=20", "ioa=100", "value=1 ")
		p.expect(5*time.Second, "type=13 ", "cot=20", "ioa=200", "value=230.5")
		p.expect(5*time.Second, "type=100", "cot=10")
	})
	t.Run("spontaneous", func(t *testing.T) {
		if err := s.Send(&iec60870.ASDU{Type: iec60870.MMeNc1, Cause: iec60870.CauseSpontaneous, CA: 1,
			Objects: []iec60870.Object{{IOA: 200, Value: 12.5}}}); err != nil {
			t.Fatal(err)
		}
		p.expect(5*time.Second, "type=13 ", "cot=3 ", "ioa=200", "value=12.5")
	})
	t.Run("clock synchronisation", func(t *testing.T) {
		p.send("clock")
		line := p.expect(5*time.Second, "SENT clock")
		f := strings.Fields(line)
		ms, _ := strconv.ParseInt(f[len(f)-1], 10, 64)
		p.expect(5*time.Second, "type=103", "cot=7 ", "neg=0")
		syncs := s.ClockSyncs()
		if len(syncs) != 1 || !syncs[0].Equal(time.UnixMilli(ms)) {
			t.Errorf("station synchronised to %v, peer sent %s", syncs, time.UnixMilli(ms).UTC())
		}
	})
	t.Run("single command select before operate", func(t *testing.T) {
		p.send("sc 5000 0 1")
		p.expect(5*time.Second, "type=45 ", "cot=7 ", "neg=0", "ioa=5000", "select=1")
		p.send("sc 5000 0 0")
		p.expect(5*time.Second, "type=45 ", "cot=7 ", "neg=0", "ioa=5000", "select=0")
		p.expect(5*time.Second, "type=1 ", "cot=11", "ioa=100", "value=0 ")
		p.expect(5*time.Second, "type=45 ", "cot=10", "neg=0", "ioa=5000")
	})
	t.Run("double command direct", func(t *testing.T) {
		p.send("dc 5001 2 0")
		p.expect(5*time.Second, "type=46 ", "cot=7 ", "neg=0", "ioa=5001")
		p.expect(5*time.Second, "type=3 ", "cot=11", "ioa=101", "value=2 ")
		p.expect(5*time.Second, "type=46 ", "cot=10", "neg=0", "ioa=5001")
	})
	t.Run("set-point select before operate", func(t *testing.T) {
		p.send("se 5002 12.25 1")
		p.expect(5*time.Second, "type=50 ", "cot=7 ", "neg=0", "ioa=5002", "select=1")
		p.send("se 5002 12.25 0")
		p.expect(5*time.Second, "type=50 ", "cot=7 ", "neg=0", "ioa=5002", "select=0")
		p.expect(5*time.Second, "type=50 ", "cot=10", "ioa=5002")
		if got := s.Get(200).Value; got != 12.25 {
			t.Errorf("measured value 200 is %v", got)
		}
	})
	t.Run("unknown object", func(t *testing.T) {
		p.send("sc 4999 1 0")
		p.expect(5*time.Second, "type=45 ", "cot=47", "neg=1", "ioa=4999")
	})
	var sel, exe int
	for _, c := range s.Commands() {
		if c.IOA == 5000 && c.Select {
			sel++
		} else if c.IOA == 5000 {
			exe++
		}
	}
	if sel != 1 || exe != 1 {
		t.Errorf("station recorded %+v", s.Commands())
	}
}

// TestLib60870ClientAgainstIEC104Sim: a lib60870 controlling station
// against the simulated IEC 104 controlled station.
func TestLib60870ClientAgainstIEC104Sim(t *testing.T) {
	needDocker(t)
	s := station104(t, nil)
	p := startPeer(t, "--add-host=host.docker.internal:host-gateway", image, "peer", "client104", "host.docker.internal", port(t, s.Addr()))
	run104(t, s, p)
}

// TestLib60870ClientAgainstIEC104SimTLS: the same over TLS 1.2 and 1.3
// with mutual authentication (crypto/tls in the simulator, mbedtls in the
// peer).
func TestLib60870ClientAgainstIEC104SimTLS(t *testing.T) {
	needDocker(t)
	k := newPKI(t)
	for _, v := range []struct {
		name string
		max  uint16
	}{{"TLS 1.2", tls.VersionTLS12}, {"TLS 1.3", tls.VersionTLS13}} {
		t.Run(v.name, func(t *testing.T) {
			s := station104(t, &tls.Config{Certificates: []tls.Certificate{k.ours}, ClientCAs: k.pool,
				ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12, MaxVersion: v.max})
			p := startPeer(t, "--add-host=host.docker.internal:host-gateway", "-v", k.dir+":/certs:ro", image,
				"peer", "client104", "host.docker.internal", port(t, s.Addr()), "tls")
			run104(t, s, p)
		})
	}
}

// TestLib60870StockClientAgainstIEC104Sim: lib60870's own cs104_client
// example (STARTDT, station interrogation, test command with time tag).
func TestLib60870StockClientAgainstIEC104Sim(t *testing.T) {
	needDocker(t)
	s := station104(t, nil)
	p := startPeer(t, "--add-host=host.docker.internal:host-gateway", image, "stdbuf", "-oL", "cs104_client",
		"host.docker.internal", port(t, s.Addr()))
	p.expect(15*time.Second, "Received STARTDT_CON")
	p.expect(15*time.Second, "RECVD ASDU type: M_SP_NA_1(1)")
	p.expect(15*time.Second, "RECVD ASDU type: M_ME_NC_1(13)")
	p.expect(15*time.Second, "RECVD ASDU type: C_TS_TA_1(107)")
	if gi, _, _, _, _ := s.Counts(); gi != 1 {
		t.Errorf("%d interrogations", gi)
	}
}

// TestLib60870MasterAgainstIEC101Sim: a lib60870 unbalanced primary on a
// pty, bridged by socat to the simulated IEC 101 line (a serial-to-IP
// converter in the simulator's terms).
func TestLib60870MasterAgainstIEC101Sim(t *testing.T) {
	needDocker(t)
	st := iec101sim.NewStation(3, 1)
	st.Set(100, iec101sim.Point{Type: iec60870.MSpNa1, Value: 1})
	st.Set(300, iec101sim.Point{Type: iec60870.MMeNc1, Value: 3.5})
	st.Controls[5000] = 100
	line := iec101sim.NewLine(false, st)
	if err := line.ListenAddr("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(line.Stop)
	p := startPeer(t, "--add-host=host.docker.internal:host-gateway", image, "serial101.sh", "master",
		"host.docker.internal", port(t, line.Addr()), "3")
	p.expect(60*time.Second, "READY")
	p.expect(15*time.Second, "LINK 3 3") // LL_STATE_AVAILABLE

	t.Run("general interrogation", func(t *testing.T) {
		p.send("gi")
		p.expect(10*time.Second, "type=100", "cot=7 ")
		p.expect(10*time.Second, "type=13 ", "cot=20", "ioa=300", "value=3.5")
		p.expect(10*time.Second, "type=100", "cot=10")
	})
	t.Run("clock synchronisation", func(t *testing.T) {
		p.send("clock")
		l := p.expect(10*time.Second, "SENT clock")
		f := strings.Fields(l)
		ms, _ := strconv.ParseInt(f[len(f)-1], 10, 64)
		p.expect(10*time.Second, "type=103", "cot=7 ", "neg=0")
		syncs := line.ClockSyncs(st)
		if len(syncs) != 1 || !syncs[0].Equal(time.UnixMilli(ms)) {
			t.Errorf("station synchronised to %v, peer sent %s", syncs, time.UnixMilli(ms).UTC())
		}
	})
	t.Run("single command select before operate", func(t *testing.T) {
		p.send("sc 5000 0 1")
		p.expect(10*time.Second, "type=45 ", "cot=7 ", "neg=0", "select=1")
		p.send("sc 5000 0 0")
		p.expect(10*time.Second, "type=45 ", "cot=7 ", "neg=0", "select=0")
		p.expect(10*time.Second, "type=45 ", "cot=10")
	})
	t.Run("spontaneous", func(t *testing.T) {
		if err := line.Spontaneous(st, &iec60870.ASDU{Type: iec60870.MMeNc1, Cause: iec60870.CauseSpontaneous,
			Objects: []iec60870.Object{{IOA: 300, Value: 4.25}}}); err != nil {
			t.Fatal(err)
		}
		p.expect(10*time.Second, "type=13 ", "cot=3 ", "ioa=300", "value=4.25")
	})
}
