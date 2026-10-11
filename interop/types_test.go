// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package interop

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec101sim"
	"github.com/cB-Sigma/iec60870-sim/sim/iec104sim"
)

// stamp is the time tag the peer sends: 2026-10-11 13:45:56.789 UTC.
var stamp = time.Date(2026, 10, 11, 13, 45, 56, 789e6, time.UTC)

func cp56Hex(t time.Time) string { return hex.EncodeToString(iec60870.CP56(t, false, time.UTC)) }

// startClient runs lib60870's controlling station against s and starts
// data transfer.
func startClient(t *testing.T, s *iec104sim.Station, extra ...string) *peer {
	t.Helper()
	args := append([]string{"--add-host=host.docker.internal:host-gateway"}, extra...)
	p := startPeer(t, append(args, image, "peer", "client104", "host.docker.internal", port(t, s.Addr()))...)
	p.expect(60*time.Second, "READY")
	p.send("startdt")
	p.expect(10*time.Second, "EVENT 2")
	return p
}

// simCommands are the command types lib60870 sends, with the value the
// simulator must record.
var simCommands = []struct {
	send  string
	typ   iec60870.TypeID
	value float64
}{
	{"type=45 v=1", iec60870.CScNa1, 1},
	{"type=46 v=2", iec60870.CDcNa1, 2},
	{"type=47 v=2", iec60870.CRcNa1, 2},
	{"type=48 v=-0.5", iec60870.CSeNa1, -0.5},
	{"type=49 v=-1200", iec60870.CSeNb1, -1200},
	{"type=50 v=7.75", iec60870.CSeNc1, 7.75},
	{"type=51 v=305419896", iec60870.CBoNa1, 0x12345678},
	{"type=58 v=0", iec60870.CScTa1, 0},
	{"type=59 v=1", iec60870.CDcTa1, 1},
	{"type=60 v=1", iec60870.CRcTa1, 1},
	{"type=61 v=0.25", iec60870.CSeTa1, 0.25},
	{"type=62 v=4096", iec60870.CSeTb1, 4096},
	{"type=63 v=-3.5", iec60870.CSeTc1, -3.5},
	{"type=64 v=1", iec60870.CBoTa1, 1},
}

// TestLib60870AllCommandsAgainstIEC104Sim: every command type with and
// without CP56Time2a from lib60870, confirmed and terminated by the
// simulated station, which records the decoded value; a time-tagged
// command older than the station's maximum delay gets no answer.
func TestLib60870AllCommandsAgainstIEC104Sim(t *testing.T) {
	needDocker(t)
	s := station104(t, nil)
	s.Lock()
	s.MaxDelay = 24 * time.Hour
	for i := range simCommands {
		s.Controls[uint32(6000+i)] = uint32(7000 + i)
	}
	s.Unlock()
	p := startClient(t, s)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, c := range simCommands {
		ioa := 6000 + i
		p.send(fmt.Sprintf("send %s ioa=%d t=%s", c.send, ioa, cp56Hex(now)))
		p.expect(5*time.Second, fmt.Sprintf("type=%d ", c.typ), "cot=7 ", "neg=0", fmt.Sprintf("ioa=%d ", ioa))
		p.expect(5*time.Second, fmt.Sprintf("type=%d ", c.typ), "cot=10 ", fmt.Sprintf("ioa=%d ", ioa))
	}
	cmds := s.Commands()
	for i, c := range simCommands {
		found := false
		for _, r := range cmds {
			if r.IOA == uint32(6000+i) {
				found = true
				if r.Type != c.typ || r.Value != c.value || r.Select {
					t.Errorf("%s: recorded %+v", c.typ, r)
				}
			}
		}
		if !found {
			t.Errorf("%s: not recorded", c.typ)
		}
	}
	t.Run("time-tagged command older than the maximum delay", func(t *testing.T) {
		p.send(fmt.Sprintf("send type=58 ioa=6000 v=1 t=%s", cp56Hex(now.Add(-48*time.Hour))))
		time.Sleep(time.Second)
		late := false
		for _, r := range s.Commands() {
			late = late || r.Late
		}
		if !late {
			t.Error("the stale command was not recorded as late")
		}
	})
}

// TestLib60870SystemCommandsAgainstIEC104Sim: group and counter
// interrogation, read, reset process, test with time tag, delay
// acquisition, parameters and clock synchronisation edges, from lib60870
// to the simulated station.
func TestLib60870SystemCommandsAgainstIEC104Sim(t *testing.T) {
	needDocker(t)
	s := station104(t, nil)
	s.Set(400, iec104sim.Point{Type: iec60870.MItNa1, Value: 777})
	s.Proc.Lock()
	s.Proc.Groups[100] = []int{2}
	s.Proc.Params[600] = 1.5
	s.Proc.Cyclic[600] = false
	s.Proc.Unlock()
	p := startClient(t, s)
	t.Run("group interrogation", func(t *testing.T) {
		p.send("send type=100 v=22")
		p.expect(5*time.Second, "type=100 ", "cot=7 ", "neg=0")
		p.expect(5*time.Second, "type=1 ", "cot=22 ", "ioa=100 ")
		p.expect(5*time.Second, "type=100 ", "cot=10 ")
	})
	t.Run("counter interrogation", func(t *testing.T) {
		p.send("send type=101 v=5")
		p.expect(5*time.Second, "type=101 ", "cot=7 ", "neg=0")
		f := kv(p.expect(5*time.Second, "type=15 ", "cot=37 ", "ioa=400 "))
		if f["value"] != "777" {
			t.Errorf("counter read %s", f["value"])
		}
		p.expect(5*time.Second, "type=101 ", "cot=10 ")
	})
	t.Run("read", func(t *testing.T) {
		p.send("send type=102 ioa=200")
		p.expect(5*time.Second, "type=13 ", "cot=5 ", "ioa=200 ", "value=230.5")
		p.send("send type=102 ioa=999")
		p.expect(5*time.Second, "type=102 ", "cot=47 ", "neg=1")
	})
	t.Run("reset process", func(t *testing.T) {
		p.send("send type=105 v=1")
		p.expect(5*time.Second, "type=105 ", "cot=7 ", "neg=0")
		p.expect(5*time.Second, "type=70 ", "cot=4 ")
	})
	t.Run("test command with time tag", func(t *testing.T) {
		p.send("send type=107 v=4660 t=" + cp56Hex(stamp))
		f := kv(p.expect(5*time.Second, "type=107 ", "cot=7 ", "neg=0"))
		if f["value"] != "4660" || f["t56"] != cp56Hex(stamp) {
			t.Errorf("test confirmation %v", f)
		}
	})
	t.Run("delay acquisition", func(t *testing.T) {
		p.send("send type=106 el=12345")
		if f := kv(p.expect(5*time.Second, "type=106 ", "cot=7 ")); f["el"] != "12345" {
			t.Errorf("delay confirmation %v", f)
		}
		p.send("send type=106 el=42 cot=3")
		deadline := time.Now().Add(5 * time.Second)
		for {
			if d, ok := s.Proc.Delay(); ok && d == 42*time.Millisecond {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the delay was not loaded")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	t.Run("parameters", func(t *testing.T) {
		p.send("send type=112 ioa=600 v=2.5 qu=1")
		p.expect(5*time.Second, "type=112 ", "cot=7 ", "neg=0", "value=2.5")
		p.send("send type=113 ioa=600 qu=3")
		p.expect(5*time.Second, "type=113 ", "cot=7 ", "neg=0")
		p.send("send type=112 ioa=601 v=1")
		p.expect(5*time.Second, "type=112 ", "cot=47 ", "neg=1")
	})
	t.Run("clock synchronisation edges", func(t *testing.T) {
		for i, c := range []struct {
			raw  string
			want time.Time
		}{
			{"e8030a8c0b0a1a", time.Date(2026, 10, 11, 12, 10, 1, 0, time.UTC)},      // SU set: ignored
			{"000000002b011a", time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC)},         // wrong day of week
			{"5fea3b17df0c63", time.Date(2099, 12, 31, 23, 59, 59, 999e6, time.UTC)}, // last millisecond
			{"00000000c10100", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)},          // year 00
			{"0000000081010c", time.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC)},          // 1 Jan 2012
		} {
			p.send("send type=103 t=" + c.raw)
			p.expect(5*time.Second, "type=103 ", "cot=7 ", "neg=0")
			syncs := s.ClockSyncs()
			if len(syncs) != i+1 || !syncs[i].Equal(c.want) {
				t.Errorf("%s: station read %v, want %s", c.raw, syncs, c.want)
			}
		}
	})
}

// TestLib60870TLSRefusedBySim: the simulated station refuses lib60870's
// certificate from an untrusted CA, and lib60870 refuses a station
// certificate it cannot verify.
func TestLib60870TLSRefusedBySim(t *testing.T) {
	needDocker(t)
	k := newPKI(t)
	t.Run("client certificate from an untrusted CA", func(t *testing.T) {
		s := station104(t, &tls.Config{Certificates: []tls.Certificate{k.ours}, ClientCAs: k.pool,
			ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})
		p := startPeer(t, "--add-host=host.docker.internal:host-gateway", "-v", k.rogueDir+":/certs:ro", image,
			"peer", "client104", "host.docker.internal", port(t, s.Addr()), "tls")
		p.send("startdt")
		p.expectAny(30*time.Second, "CONNECT-FAILED", "EVENT 1", "EVENT 4")
		if _, _, _, _, startDT := s.Counts(); startDT != 0 {
			t.Errorf("the station received %d STARTDT", startDT)
		}
	})
	t.Run("station certificate from an untrusted CA", func(t *testing.T) {
		s := station104(t, &tls.Config{Certificates: []tls.Certificate{k.rogue}, ClientCAs: k.pool,
			ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})
		p := startPeer(t, "--add-host=host.docker.internal:host-gateway", "-v", k.dir+":/certs:ro", image,
			"peer", "client104", "host.docker.internal", port(t, s.Addr()), "tls")
		p.expectAny(30*time.Second, "CONNECT-FAILED", "EVENT 1", "EVENT 4")
		if _, _, _, _, startDT := s.Counts(); startDT != 0 {
			t.Errorf("the station received %d STARTDT", startDT)
		}
	})
}

// TestLib60870RedundancyAgainstIEC104Sim: two lib60870 controlling
// stations on one simulated station; starting the second closes the first
// (one started connection per redundancy group).
func TestLib60870RedundancyAgainstIEC104Sim(t *testing.T) {
	needDocker(t)
	s := station104(t, nil)
	a := startClient(t, s)
	b := startPeer(t, "--add-host=host.docker.internal:host-gateway", image, "peer", "client104", "host.docker.internal", port(t, s.Addr()))
	b.expect(60*time.Second, "READY")
	b.send("startdt")
	b.expect(10*time.Second, "EVENT 2")
	a.expect(10*time.Second, "EVENT 1") // closed
	if err := s.Send(&iec60870.ASDU{Type: iec60870.MMeNc1, Cause: iec60870.CauseSpontaneous, CA: 1,
		Objects: []iec60870.Object{{IOA: 200, Value: 1.25}}}); err != nil {
		t.Fatal(err)
	}
	b.expect(5*time.Second, "type=13 ", "cot=3 ", "value=1.25")
}

// TestLib60870MasterLineConfigurations: lib60870 as IEC 101 controlling
// station, balanced and unbalanced, with each field size, against the
// simulated line: interrogation, a command with select-before-operate,
// clock synchronisation and spontaneous data.
func TestLib60870MasterLineConfigurations(t *testing.T) {
	needDocker(t)
	for _, c := range []struct {
		balanced         bool
		la, cot, ca, ioa int
	}{
		{false, 2, 2, 2, 3}, {false, 1, 1, 2, 1}, {true, 0, 2, 1, 2}, {true, 1, 1, 1, 2}, {true, 2, 2, 2, 3},
	} {
		name := fmt.Sprintf("balanced %v la %d cot %d ca %d ioa %d", c.balanced, c.la, c.cot, c.ca, c.ioa)
		t.Run(name, func(t *testing.T) {
			st := iec101sim.NewStation(3, 1)
			st.Params = iec60870.Params{COTSize: c.cot, CASize: c.ca, IOASize: c.ioa}
			st.Set(100, iec101sim.Point{Type: iec60870.MSpNa1, Value: 1})
			st.Set(30, iec101sim.Point{Type: iec60870.MMeNc1, Value: 3.5})
			st.Controls[50] = 100
			line := iec101sim.NewLine(c.balanced, st)
			line.ASize = c.la
			if err := line.ListenAddr("127.0.0.1:0"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(line.Stop)
			b := "0"
			if c.balanced {
				b = "1"
			}
			p := startPeer(t, "--add-host=host.docker.internal:host-gateway", image, "serial101.sh", "master",
				"host.docker.internal", port(t, line.Addr()), "3", "bal="+b, "la="+strconv.Itoa(c.la), "cot="+strconv.Itoa(c.cot),
				"ca="+strconv.Itoa(c.ca), "ioa="+strconv.Itoa(c.ioa))
			p.expect(60*time.Second, "READY")
			p.send("gi")
			p.expect(15*time.Second, "type=100", "cot=7 ")
			p.expect(10*time.Second, "type=13 ", "cot=20", "ioa=30 ", "value=3.5")
			p.expect(10*time.Second, "type=100", "cot=10")
			p.send("sc 50 0 1")
			p.expect(10*time.Second, "type=45 ", "cot=7 ", "neg=0", "select=1")
			p.send("sc 50 0 0")
			p.expect(10*time.Second, "type=45 ", "cot=7 ", "neg=0", "select=0")
			p.expect(10*time.Second, "type=45 ", "cot=10")
			p.send("clock")
			l := p.expect(10*time.Second, "SENT clock")
			ms, _ := strconv.ParseInt(l[len("SENT clock "):], 10, 64)
			p.expect(10*time.Second, "type=103", "cot=7 ", "neg=0")
			if syncs := line.ClockSyncs(st); len(syncs) != 1 || !syncs[0].Equal(time.UnixMilli(ms)) {
				t.Errorf("station synchronised to %v, peer sent %s", syncs, time.UnixMilli(ms).UTC())
			}
			if err := line.Spontaneous(st, &iec60870.ASDU{Type: iec60870.MMeTf1, Cause: iec60870.CauseSpontaneous,
				Objects: []iec60870.Object{{IOA: 30, Value: 4.25, Time: stamp}}}); err != nil {
				t.Fatal(err)
			}
			f := kv(p.expect(10*time.Second, "type=36 ", "cot=3 ", "ioa=30 ", "value=4.25"))
			if f["t56"] != cp56Hex(stamp) {
				t.Errorf("time tag %s, want %s", f["t56"], cp56Hex(stamp))
			}
		})
	}
}
