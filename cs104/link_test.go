package cs104_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec104sim"
	"github.com/cB-Sigma/iec60870/cs104"
)

func startSim(t *testing.T) *iec104sim.Station {
	t.Helper()
	s := iec104sim.New(1)
	if err := s.Listen("", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s
}

func dial(t *testing.T, s *iec104sim.Station, cfg cs104.LinkConfig) *cs104.Link {
	t.Helper()
	l, err := cs104.Dial(context.Background(), s.Addr(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

func spont(ioa uint32, v float64) *iec60870.ASDU {
	return &iec60870.ASDU{Type: iec60870.MMeNc1, Cause: iec60870.CauseSpontaneous, Objects: []iec60870.Object{{IOA: ioa, Value: v}}}
}

func gi(t *testing.T) []byte {
	b, err := (&iec60870.ASDU{Type: iec60870.CIcNa1, Cause: iec60870.CauseActivation, CA: 1, Objects: []iec60870.Object{{Value: 20}}}).Encode(iec60870.Params{}.WithDefaults(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Covers: SIG-IPE-144
func TestLinkWindowsAndTimers(t *testing.T) {
	t.Run("k: at most k unacknowledged I-frames, then t1 closes the link", func(t *testing.T) {
		s := startSim(t)
		s.Lock()
		s.NoAck, s.SilentCommands = true, true
		s.Unlock()
		l := dial(t, s, cs104.LinkConfig{K: 3, W: 2, T1: 400 * time.Millisecond, T2: 200 * time.Millisecond})
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		sent := 0
		for i := 0; i < 5; i++ {
			// Only k sends complete; the fourth waits for the window.
			go func() { _ = l.Send(ctx, gi(t)) }()
		}
		eventually(t, 2*time.Second, "k frames on the wire", func() bool {
			_, _, _, sent, _ = s.Counts()
			return sent == 3
		})
		time.Sleep(100 * time.Millisecond)
		if _, _, _, sent, _ = s.Counts(); sent != 3 {
			t.Errorf("the station received %d I-frames, want k=3", sent)
		}
		if st := l.Stats(); st.MaxOutstanding != 3 {
			t.Errorf("most unacknowledged = %d, want 3", st.MaxOutstanding)
		}
		select {
		case <-l.Done():
			if !strings.Contains(l.Err().Error(), "t1") {
				t.Errorf("link ended with %v, want a t1 expiry", l.Err())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("t1 did not close the link")
		}
	})
	t.Run("w: acknowledge after w received I-frames", func(t *testing.T) {
		s := startSim(t)
		l := dial(t, s, cs104.LinkConfig{K: 12, W: 4, T1: 5 * time.Second, T2: 4 * time.Second})
		for i := 0; i < 8; i++ {
			if err := s.Send(spont(uint32(i), float64(i))); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 8; i++ {
			select {
			case <-l.ASDUs():
			case <-time.After(2 * time.Second):
				t.Fatal("ASDU not received")
			}
		}
		eventually(t, time.Second, "two S-frames", func() bool {
			_, _, sf, _, _ := s.Counts()
			return sf == 2
		})
	})
	t.Run("t2: acknowledge a lone I-frame within t2", func(t *testing.T) {
		s := startSim(t)
		l := dial(t, s, cs104.LinkConfig{K: 12, W: 8, T1: time.Second, T2: 300 * time.Millisecond})
		start := time.Now()
		_ = s.Send(spont(1, 1))
		<-l.ASDUs()
		eventually(t, 2*time.Second, "an S-frame", func() bool {
			_, _, sf, _, _ := s.Counts()
			return sf == 1
		})
		if d := time.Since(start); d < 250*time.Millisecond {
			t.Errorf("acknowledged after %s, before t2", d)
		}
	})
	t.Run("t3: TESTFR on an idle link", func(t *testing.T) {
		s := startSim(t)
		l := dial(t, s, cs104.LinkConfig{T1: time.Second, T2: 500 * time.Millisecond, T3: 200 * time.Millisecond})
		eventually(t, 3*time.Second, "two test frames", func() bool { return l.Stats().TestFRSent >= 2 })
		select {
		case <-l.Done():
			t.Fatalf("the link closed although TESTFR was confirmed: %v", l.Err())
		default:
		}
	})
	t.Run("t1: STARTDT without confirmation fails", func(t *testing.T) {
		s := startSim(t)
		s.Lock()
		s.NoStartDTCon = true
		s.Unlock()
		start := time.Now()
		_, err := cs104.Dial(context.Background(), s.Addr(), cs104.LinkConfig{T1: 300 * time.Millisecond, T2: 100 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "STARTDT") {
			t.Errorf("Dial = %v, want a STARTDT failure", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("took %s", d)
		}
	})
	t.Run("parameter relations", func(t *testing.T) {
		// w above 2/3 k is a recommendation (CheckWindow), not a range.
		if c := (cs104.LinkConfig{K: 12, W: 10}).WithDefaults(); c.Validate() != nil || c.CheckWindow() == nil {
			t.Error("w above 2/3 k: Validate refuses it or CheckWindow misses it")
		}
		if err := (cs104.LinkConfig{T1: time.Second, T2: 2 * time.Second}).WithDefaults().Validate(); err == nil {
			t.Error("t2 >= t1 was accepted")
		}
	})
}
