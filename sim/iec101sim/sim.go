// Package iec101sim is an in-process IEC 60870-5-101 controlled station
// for tests, reached over TCP the way a transparent serial-to-IP converter
// is: FT1.2 frames on a byte stream. It answers as an unbalanced
// secondary (status, reset, class 1 and class 2 requests, FCB repeats) or
// as a balanced station (it also starts its own link and sends its data as
// user data with confirmation). Interrogation, clock synchronisation and
// commands with select-before-operate and deactivation of a select are
// served from a point database; NoActTerm withholds activation
// terminations. Busy (DFC), NotFunctioning (FC14) and DropAnswers exercise
// the link procedures of IEC 60870-5-101 Figures 5 and 6; the other
// procedures of clause 7.4 (groups, counters with freeze, read, reset,
// test, delay acquisition, parameters, files) are iec104sim.Procedures.
package iec101sim

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec104sim"
	"github.com/cB-Sigma/iec60870/cs101"
	"github.com/cB-Sigma/iec60870/sec"
)

// Point is one monitored object.
type Point struct {
	Type    iec60870.TypeID
	Value   float64
	Quality uint8
}

// Command is a command the station received.
type Command struct {
	Type   iec60870.TypeID
	IOA    uint32
	Value  float64
	Select bool
	Cause  iec60870.Cause
}

// Station is one secondary station on the simulated line.
type Station struct {
	LinkAddress uint16
	CA          uint16
	Params      iec60870.Params
	Loc         *time.Location
	// Controls maps a command IOA to the monitored IOA it changes.
	Controls map[uint32]uint32
	// Behaviour switches (under Line.Lock).
	RefuseSelect  bool
	RefuseExecute bool
	Silent        bool // answer nothing
	NoActTerm     bool // confirm executes but never terminate them
	// Busy answers user data with NACK and DFC (not accepted) and status
	// requests with DFC while it counts down, one per status request.
	Busy int
	// NotFunctioning answers every request with FC14 (link service not
	// functioning).
	NotFunctioning bool
	// DropAnswers drops the next answers after serving the request, so the
	// controlling station repeats it (same FCB).
	DropAnswers int
	// Proc answers the procedures beyond interrogation and commands.
	Proc *iec104sim.Procedures
	// Security, when set before Listen, runs the IEC TS 60870-5-7
	// A-Profile (IEC 62351-5) as the controlled station: every ASDU is
	// Secure Data and the key management messages are answered.
	Security *sec.SecureLink
	secCh    chan []sec.Frame

	points   map[uint32]*Point
	class1   [][]byte
	class2   [][]byte
	commands []Command
	selected map[uint32]bool
	clock    []time.Time
	gi, ci   int
	resets   int
	polls    int
	lastFCB  int8 // -1 after reset
	last     []byte
}

// NewStation creates a station with IEC 101's usual sizes (cause 1, CA 1,
// IOA 2).
func NewStation(link, ca uint16) *Station {
	return &Station{LinkAddress: link, CA: ca, Params: iec60870.Params{COTSize: 1, CASize: 1, IOASize: 2}, Loc: time.UTC,
		Controls: map[uint32]uint32{}, points: map[uint32]*Point{}, lastFCB: -1, Proc: iec104sim.NewProcedures()}
}

// Line is a simulated serial line with one or more stations.
type Line struct {
	Balanced bool
	ASize    int // link address size (default 1)

	mu       sync.Mutex
	stations map[uint16]*Station
	ln       net.Listener
	addr     string
	conn     net.Conn
	frames   []cs101.Frame // every frame received
	// balanced primary side
	resp   chan cs101.Frame
	ourFCB bool
	linkUp bool
	wmu    sync.Mutex
	lmu    sync.Mutex // one balanced primary exchange sequence at a time
	// A-Profile timers and message queues of the stations.
	secDone chan struct{}
	secStop func()
}

// NewLine creates a line.
func NewLine(balanced bool, stations ...*Station) *Line {
	l := &Line{Balanced: balanced, ASize: 1, stations: map[uint16]*Station{}, resp: make(chan cs101.Frame, 4)}
	for _, s := range stations {
		l.stations[s.LinkAddress] = s
	}
	return l
}

// Listen accepts one controlling station at a time on 127.0.0.1.
func (l *Line) Listen() error { return l.ListenAddr("127.0.0.1:0") }

// ListenAddr accepts one controlling station at a time on addr, the way a
// serial-to-IP converter in server mode does.
func (l *Line) ListenAddr(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.ln, l.addr = ln, ln.Addr().String()
	if l.secDone == nil {
		l.secDone = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		l.secStop = cancel
		for _, s := range l.stations {
			if s.Security != nil {
				s.secCh = make(chan []sec.Frame, 64)
				go s.Security.Run(ctx)
				go l.secQueue(s, l.secDone)
			}
		}
	}
	l.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.mu.Lock()
			if l.conn != nil {
				_ = l.conn.Close()
			}
			l.conn = c
			l.linkUp = false
			l.mu.Unlock()
			go l.serve(c)
		}
	}()
	return nil
}

// Addr is the listening address.
func (l *Line) Addr() string { l.mu.Lock(); defer l.mu.Unlock(); return l.addr }

// Stop closes the listener and the connection.
func (l *Line) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.secStop != nil {
		l.secStop()
		close(l.secDone)
		l.secStop, l.secDone = nil, nil
	}
	if l.ln != nil {
		_ = l.ln.Close()
	}
	if l.conn != nil {
		_ = l.conn.Close()
	}
}

// Drop closes the current connection only.
func (l *Line) Drop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		_ = l.conn.Close()
	}
}

// Lock and Unlock guard the stations' switches.
func (l *Line) Lock() { l.mu.Lock() }

// Unlock releases Lock.
func (l *Line) Unlock() { l.mu.Unlock() }

// Set stores a point.
func (s *Station) Set(ioa uint32, p Point) { cp := p; s.points[ioa] = &cp }

// Frames returns every frame received.
func (l *Line) Frames() []cs101.Frame {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]cs101.Frame(nil), l.frames...)
}

// Commands returns the commands a station received.
func (l *Line) Commands(s *Station) []Command {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Command(nil), s.commands...)
}

// Counts returns interrogations, counter interrogations, link resets and
// class polls a station received.
func (l *Line) Counts(s *Station) (gi, ci, resets, polls int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return s.gi, s.ci, s.resets, s.polls
}

// ClockSyncs returns the times received in clock synchronisations.
func (l *Line) ClockSyncs(s *Station) []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), s.clock...)
}

// Spontaneous queues an ASDU as class 1 data (unbalanced) or sends it at
// once (balanced).
func (l *Line) Spontaneous(s *Station, a *iec60870.ASDU) error {
	if a.CA == 0 {
		a.CA = s.CA
	}
	b, err := a.Encode(s.Params, s.Loc)
	if err != nil {
		return err
	}
	l.mu.Lock()
	s.enqueue(b)
	l.mu.Unlock()
	if l.Balanced {
		go l.flush(s)
	}
	return nil
}

func (l *Line) write(f cs101.Frame) {
	l.mu.Lock()
	c := l.conn
	l.mu.Unlock()
	if c == nil {
		return
	}
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_, _ = c.Write(f.Encode(l.ASize))
}

func (l *Line) serve(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		f, err := cs101.ReadFrame(r, l.ASize)
		if errors.Is(err, cs101.ErrFrame) {
			continue
		}
		if err != nil {
			return
		}
		l.mu.Lock()
		l.frames = append(l.frames, f)
		l.mu.Unlock()
		if !f.PRM() {
			select {
			case l.resp <- f:
			default:
			}
			continue
		}
		l.mu.Lock()
		if f.Addr == uint16(1)<<(8*l.ASize)-1 && f.Fn() == cs101.FnUserDataNoReply && f.ASDU != nil {
			// A broadcast (6.2.1.1): every station takes it, none answers.
			for _, st := range l.stations {
				l.userData(st, f.ASDU)
			}
			l.mu.Unlock()
			continue
		}
		s := l.stations[f.Addr]
		silent := s == nil || s.Silent
		l.mu.Unlock()
		if silent {
			continue
		}
		l.secondary(s, f)
	}
}

// secondary answers a request from the controlling station.
func (l *Line) secondary(s *Station, f cs101.Frame) {
	l.mu.Lock()
	fcv := f.C&cs101.CtrlFCV != 0
	bit := int8(0)
	if f.C&cs101.CtrlFCB != 0 {
		bit = 1
	}
	if fcv && s.lastFCB == bit && s.last != nil {
		// A repeat: the previous answer was lost; send it again.
		last := s.last
		c := l.conn
		l.mu.Unlock()
		if c != nil {
			l.wmu.Lock()
			_, _ = c.Write(last)
			l.wmu.Unlock()
		}
		return
	}
	if fcv && s.Busy == 0 {
		s.lastFCB = bit
	}
	ans := cs101.Frame{Addr: s.LinkAddress}
	var after func()
	switch {
	case s.NotFunctioning:
		ans.C = cs101.FnNotFunctioning
	case s.Busy > 0 && f.Fn() == cs101.FnUserDataConfirm:
		// Figure 6, busy: user data is not accepted.
		ans.C = cs101.FnNack | cs101.CtrlDFC
	case s.Busy > 0 && f.Fn() == cs101.FnRequestStatus:
		s.Busy--
		ans.C = cs101.FnStatus | cs101.CtrlDFC
	default:
		after = l.answer(s, f, &ans)
	}
	if !l.Balanced && len(s.class1) > 0 {
		ans.C |= cs101.CtrlACD
	}
	var out []byte
	if !l.Balanced && ans.C == cs101.FnNoData && len(s.class1) == 0 && f.Fn() != cs101.FnRequestStatus {
		out = []byte{cs101.SingleACK}
	} else {
		out = ans.Encode(l.ASize)
	}
	if fcv {
		s.last = out
	}
	c := l.conn
	drop := s.DropAnswers > 0
	if drop {
		s.DropAnswers--
	}
	l.mu.Unlock()
	if c != nil && !drop {
		l.wmu.Lock()
		_, _ = c.Write(out)
		l.wmu.Unlock()
	}
	if after != nil {
		after()
	}
}

// answer fills the answer to a request (under mu) and returns what to do
// after sending it.
func (l *Line) answer(s *Station, f cs101.Frame, ans *cs101.Frame) (after func()) {
	switch f.Fn() {
	case cs101.FnRequestStatus:
		ans.C = cs101.FnStatus
	case cs101.FnResetLink:
		s.lastFCB, s.last = -1, nil
		s.resets++
		ans.C = cs101.FnAck
		if s.Security != nil {
			ch := s.secCh
			s.Security.Attach(s, func(f []sec.Frame) {
				select {
				case ch <- f:
				default:
				}
			})
		}
		if l.Balanced {
			after = func() { go l.startOwnLink(s) }
		}
	case cs101.FnTestLink:
		ans.C = cs101.FnAck
	case cs101.FnUserDataConfirm, cs101.FnUserDataNoReply:
		ans.C = cs101.FnAck
		l.userData(s, f.ASDU)
		if l.Balanced {
			after = func() { go l.flush(s) }
		}
	case cs101.FnRequestClass1, cs101.FnRequestClass2:
		s.polls++
		q := &s.class2
		if f.Fn() == cs101.FnRequestClass1 || len(s.class2) == 0 {
			q = &s.class1
		}
		if f.Fn() == cs101.FnRequestClass2 && len(s.class1) > 0 && len(s.class2) == 0 {
			q = nil // announce class 1 with ACD, no data
		}
		if q != nil && len(*q) > 0 {
			ans.C, ans.ASDU = cs101.FnUserData, (*q)[0]
			*q = (*q)[1:]
		} else {
			ans.C = cs101.FnNoData
		}
	default:
		ans.C = cs101.FnNotImplemented
	}
	return after
}

// enqueue queues an application ASDU as class 1 data, as Secure Data with
// the A-Profile (under mu).
func (s *Station) enqueue(b []byte) {
	if s.Security == nil {
		s.class1 = append(s.class1, b)
		return
	}
	frames, err := s.Security.Wrap(b)
	if err == nil {
		s.class1 = append(s.class1, frames...)
	}
}

// secQueue moves the A-Profile's own messages into class 1.
func (l *Line) secQueue(s *Station, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case frames := <-s.secCh:
			l.mu.Lock()
			for _, f := range frames {
				s.class1 = append(s.class1, f.Data)
			}
			l.mu.Unlock()
			if l.Balanced {
				go l.flush(s)
			}
		}
	}
}

// userData serves an ASDU from the controlling station (under mu).
func (l *Line) userData(s *Station, b []byte) {
	if s.Security != nil {
		data, ok := s.Security.Unwrap(b)
		if !ok {
			return
		}
		b = data
	}
	a, err := iec60870.DecodeASDU(b, s.Params, s.Loc)
	if err != nil {
		return
	}
	if len(a.Objects) == 0 && !a.Type.File() {
		return
	}
	reply := func(r *iec60870.ASDU) {
		r.CA = s.CA
		if enc, err := r.Encode(s.Params, s.Loc); err == nil {
			s.enqueue(enc)
		}
	}
	con := func(neg bool) {
		r := *a
		r.Cause, r.Negative = iec60870.CauseActCon, neg
		reply(&r)
	}
	var ioas []uint32
	for ioa := range s.points {
		ioas = append(ioas, ioa)
	}
	get := func(ioa uint32) (iec60870.TypeID, float64, uint8, bool) {
		p, ok := s.points[ioa]
		if !ok {
			return 0, 0, 0, false
		}
		return p.Type, p.Value, p.Quality, true
	}
	if s.Proc.Handle(a, ioas, get, reply) {
		return
	}
	o := a.Objects[0]
	switch a.Type {
	case iec60870.CIcNa1:
		s.gi++
		con(false)
		for ioa, p := range s.points {
			reply(&iec60870.ASDU{Type: p.Type, Cause: iec60870.CauseInterrogated, Objects: []iec60870.Object{{IOA: ioa, Value: p.Value, Quality: p.Quality}}})
		}
		r := *a
		r.Cause = iec60870.CauseActTerm
		reply(&r)
	case iec60870.CCiNa1:
		s.ci++
		con(false)
	case iec60870.CCsNa1:
		s.clock = append(s.clock, o.Time)
		con(false)
	default:
		s.commands = append(s.commands, Command{Type: a.Type, IOA: o.IOA, Value: o.Value, Select: o.Select, Cause: a.Cause})
		if s.selected == nil {
			s.selected = map[uint32]bool{}
		}
		if a.Cause == iec60870.CauseDeactivation {
			// Cancelling a select: confirmed when one is pending.
			r := *a
			r.Cause, r.Negative = iec60870.CauseDeactCon, !(o.Select && s.selected[o.IOA])
			delete(s.selected, o.IOA)
			reply(&r)
			return
		}
		mon, ok := s.Controls[o.IOA]
		if !ok {
			r := *a
			r.Cause, r.Negative = iec60870.CauseUnknownObject, true
			reply(&r)
			return
		}
		if o.Select && s.RefuseSelect || !o.Select && s.RefuseExecute {
			con(true)
			return
		}
		con(false)
		if o.Select {
			s.selected[o.IOA] = true
		} else {
			delete(s.selected, o.IOA)
			if p := s.points[mon]; p != nil {
				p.Value = o.Value
				reply(&iec60870.ASDU{Type: p.Type, Cause: iec60870.CauseReturnRemote, Objects: []iec60870.Object{{IOA: mon, Value: p.Value}}})
			}
			if !s.NoActTerm {
				r := *a
				r.Cause = iec60870.CauseActTerm
				reply(&r)
			}
		}
	}
}

// primary sends a request as the balanced station and waits for the
// answer.
func (l *Line) primary(s *Station, fn byte, fcv bool, asdu []byte) (cs101.Frame, bool) {
	c := cs101.CtrlPRM | fn
	l.mu.Lock()
	if fcv {
		c |= cs101.CtrlFCV
		if l.ourFCB {
			c |= cs101.CtrlFCB
		}
	}
	l.mu.Unlock()
	for try := 0; try < 3; try++ {
		for drained := false; !drained; {
			select {
			case <-l.resp:
			default:
				drained = true
			}
		}
		l.write(cs101.Frame{C: c, Addr: s.LinkAddress, ASDU: asdu})
		select {
		case r := <-l.resp:
			if fcv {
				l.mu.Lock()
				l.ourFCB = !l.ourFCB
				l.mu.Unlock()
			}
			return r, true
		case <-time.After(500 * time.Millisecond):
		}
	}
	return cs101.Frame{}, false
}

func (l *Line) startOwnLink(s *Station) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	if _, ok := l.primary(s, cs101.FnRequestStatus, false, nil); !ok {
		return
	}
	if _, ok := l.primary(s, cs101.FnResetLink, false, nil); !ok {
		return
	}
	l.mu.Lock()
	l.ourFCB, l.linkUp = true, true
	l.mu.Unlock()
	l.flushLocked(s)
}

// flush sends queued data as user data with confirmation (balanced).
func (l *Line) flush(s *Station) {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	l.flushLocked(s)
}

func (l *Line) flushLocked(s *Station) {
	for {
		l.mu.Lock()
		if !l.linkUp || len(s.class1) == 0 {
			l.mu.Unlock()
			return
		}
		b := s.class1[0]
		l.mu.Unlock()
		if _, ok := l.primary(s, cs101.FnUserDataConfirm, true, b); !ok {
			return
		}
		l.mu.Lock()
		if len(s.class1) > 0 {
			s.class1 = s.class1[1:]
		}
		l.mu.Unlock()
	}
}
