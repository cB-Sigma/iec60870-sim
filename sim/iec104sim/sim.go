// Package iec104sim is an in-process IEC 60870-5-104 controlled station
// for tests: a point database answered by general and counter
// interrogation, spontaneous data, clock synchronisation and commands with
// select-before-operate and deactivation of a select, plus switches that
// make it misbehave (no acknowledgements, no STARTDT confirmation, refused
// commands, no activation termination). Connections follow the controlled
// station's start/stop state machine of IEC 60870-5-104 Figures 17 and 36:
// stopped until STARTDT (U-frames only; an I- or S-frame closes the
// connection), STOPDT confirmed only once every I-frame sent is
// acknowledged, and a STARTDT on one connection closes the others that are
// not stopped (one started connection per redundancy group). The
// procedures of IEC 60870-5-101 clause 7.4 beyond interrogation and
// commands are answered by Procedures (procs.go).
package iec104sim

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870/cs104"
	"github.com/cB-Sigma/iec60870/sec"
	"github.com/cB-Sigma/iec60870/station"

	"github.com/cB-Sigma/iotsec"
)

// Point is one monitored object.
type Point struct {
	Type    iec60870.TypeID // the M_ type sent in interrogation replies
	Value   float64
	Quality uint8
}

// Command is a command or set-point the station received.
type Command struct {
	Type   iec60870.TypeID
	IOA    uint32
	Value  float64
	Select bool
	Cause  iec60870.Cause
	// Late marks a time-tagged command older than MaxDelay: recorded, not
	// executed and not answered (IEC 60870-5-104 clause 8, Amendment 1).
	Late bool
}

// Station is the simulator.
type Station struct {
	CA     uint16
	Params iec60870.Params
	Loc    *time.Location
	// Behaviour switches, safe to change while running (under Lock).
	NoAck          bool // never acknowledge or answer received I-frames
	NoStartDTCon   bool // never confirm STARTDT
	RefuseSelect   bool // negative confirmation of selects
	RefuseExecute  bool // negative confirmation of executes
	RequireSelect  bool // an execute without a preceding select is refused
	SilentCommands bool // never answer commands
	NoActTerm      bool // confirm executes but never terminate them
	NegActTerm     bool // terminate executes negatively (the operation failed)
	// ConfirmDelay holds the answers to each command this long (a slow
	// RTU: interlock evaluation, a busy bay controller); answers keep
	// their order.
	ConfirmDelay time.Duration
	// MaxDelay, when set, is the maximum allowable delay of time-tagged
	// commands (types 58 to 64); older ones get no reply.
	MaxDelay time.Duration
	// TestMismatch answers a test command (C_TS_TA_1) with another counter.
	TestMismatch bool
	// Controls maps a command IOA to the monitored IOA it changes.
	Controls map[uint32]uint32
	// TLS, when set before Listen, secures every accepted connection (an
	// IEC 62351-3 server profile, say); it takes precedence over the cfg
	// argument.
	TLS iotsec.TLSServer
	// Proc answers the procedures beyond interrogation and commands.
	Proc *Procedures
	// Security, when set before Listen, runs the IEC TS 60870-5-7
	// A-Profile (IEC 62351-5) as the controlled station: every ASDU is
	// Secure Data and the key management messages are answered.
	Security *sec.SecureLink
	// TamperNext changes one octet of the next Secure Data sent (the
	// receiver must refuse it); lastSecure keeps the frames of the last one
	// for ReplayLast.
	TamperNext bool
	lastSecure [][]byte

	mu     sync.Mutex
	ln     net.Listener
	addr   string
	tlsCfg *tls.Config
	// st is the library's controlled station: the point database,
	// interrogation, clock synchronisation and commands; the switches
	// above reach it through its hooks.
	st       *station.Station
	conns    map[*conn]bool
	commands []Command
	requests []Command // every ASDU received: type, cause, first IOA
	clock    []time.Time
	gi, ci   int
	sRecv    int // S-frames received
	iRecv    int
	startDT  int
	testFR   int
	accepts  int
	closed   bool
	secStop  func()
}

// New creates a station with common address ca (default parameters).
func New(ca uint16) *Station {
	s := &Station{CA: ca, Params: iec60870.Params{}.WithDefaults(), Loc: time.UTC,
		conns: map[*conn]bool{}, Controls: map[uint32]uint32{}, Proc: NewProcedures()}
	s.st = station.New(station.Config{CA: ca, ControlFor: s.control, Intercept: s.intercept})
	return s
}

// sector is the library sector at the station's (current) common address.
func (s *Station) sector() *station.Sector {
	s.mu.Lock()
	ca := s.CA
	s.mu.Unlock()
	return s.st.SectorAt(ca)
}

// control turns Controls and the command switches into the library's
// control of a command address.
func (s *Station) control(_ uint16, ioa uint32) (station.Control, bool) {
	s.mu.Lock()
	target, ok := s.Controls[ioa]
	reqSel, noTerm, neg := s.RequireSelect, s.NoActTerm, s.NegActTerm
	s.mu.Unlock()
	if !ok {
		return station.Control{}, false
	}
	wait := func() {
		s.mu.Lock()
		d := s.ConfirmDelay
		s.mu.Unlock()
		time.Sleep(d)
	}
	return station.Control{IOA: ioa, SelectRequired: reqSel, NoTermination: noTerm,
		Select: func(context.Context, station.Command) error {
			wait()
			s.mu.Lock()
			refuse := s.RefuseSelect
			s.mu.Unlock()
			if refuse {
				return errors.New("select refused")
			}
			return nil
		},
		Execute: func(_ context.Context, c station.Command, confirm func()) error {
			wait()
			s.mu.Lock()
			refuse := s.RefuseExecute
			s.mu.Unlock()
			if refuse {
				return errors.New("execute refused")
			}
			confirm()
			sec := s.sector()
			if p, ok := sec.Get(target); ok {
				o := p.Object
				o.Value, o.Time = c.Value, time.Time{}
				_ = sec.ReturnInfo(o, false)
			}
			if neg {
				return errors.New("the operation failed")
			}
			return nil
		}}, true
}

// Listen starts accepting on 127.0.0.1 (a free port unless addr is set),
// with TLS when cfg is not nil.
func (s *Station) Listen(addr string, cfg *tls.Config) error {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var ln net.Listener
	var err error
	if s.TLS != nil {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			ln = iotsec.TLSListener(ln, s.TLS)
		}
	} else if cfg != nil {
		ln, err = tls.Listen("tcp", addr, cfg)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln, s.addr, s.tlsCfg, s.closed = ln, ln.Addr().String(), cfg, false
	if s.Security != nil && s.secStop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.secStop = cancel
		go s.Security.Run(ctx)
	}
	s.mu.Unlock()
	go s.accept(ln)
	return nil
}

// Addr is the listening address.
func (s *Station) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop closes the listener and every connection.
func (s *Station) Stop() {
	s.mu.Lock()
	s.closed = true
	if s.secStop != nil {
		s.secStop()
		s.secStop = nil
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
	cs := s.conns
	s.conns = map[*conn]bool{}
	s.mu.Unlock()
	for c := range cs {
		_ = c.c.Close()
	}
}

// Restart listens again on the same address.
func (s *Station) Restart() error { return s.Listen(s.Addr(), s.tlsCfg) }

// Lock and Unlock guard the behaviour switches.
func (s *Station) Lock() { s.mu.Lock() }

// Unlock releases Lock.
func (s *Station) Unlock() { s.mu.Unlock() }

// Set stores a point.
func (s *Station) Set(ioa uint32, p Point) {
	s.sector().Add(station.Point{Type: p.Type, Object: iec60870.Object{IOA: ioa, Value: p.Value, Quality: p.Quality}})
}

// Get returns a point.
func (s *Station) Get(ioa uint32) Point {
	if p, ok := s.sector().Get(ioa); ok {
		return Point{Type: p.Type, Value: p.Value, Quality: p.Quality}
	}
	return Point{}
}

// Library returns the library station the simulator runs on: its sectors,
// sessions and statistics.
func (s *Station) Library() *station.Station { return s.st }

// Commands returns the commands received so far.
func (s *Station) Commands() []Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Command(nil), s.commands...)
}

// Requests returns the type, cause and first address of every ASDU the
// station received.
func (s *Station) Requests() []Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Command(nil), s.requests...)
}

// ClockSyncs returns the times received in clock synchronisations.
func (s *Station) ClockSyncs() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.clock...)
}

// Counts returns how many general and counter interrogations, S-frames,
// I-frames and STARTDTs the station received.
func (s *Station) Counts() (gi, ci, sFrames, iFrames, startDT int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gi, s.ci, s.sRecv, s.iRecv, s.startDT
}

// Links returns how many connections were accepted, how many are open and
// started, and how many TESTFR acts arrived (on any connection).
func (s *Station) Links() (accepted, started, testFR int) {
	s.mu.Lock()
	var cs []*conn
	for c := range s.conns {
		cs = append(cs, c)
	}
	accepted, testFR = s.accepts, s.testFR
	s.mu.Unlock()
	// A connection's write lock is taken before the station's, never after.
	for _, c := range cs {
		if c.state.Load() == stStarted {
			started++
		}
	}
	return accepted, started, testFR
}

// DropConnections closes every open connection but keeps listening.
func (s *Station) DropConnections() {
	s.mu.Lock()
	var cs []*conn
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		_ = c.c.Close()
	}
}

// Connections is the number of open connections.
func (s *Station) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// ReplayLast sends the frames of the last Secure Data again, unchanged (a
// replay the receiver must refuse).
func (s *Station) ReplayLast() error {
	s.mu.Lock()
	frames := s.lastSecure
	var cs []*conn
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	if frames == nil {
		return errors.New("no secure data sent yet")
	}
	for _, c := range cs {
		for _, f := range frames {
			_ = c.sendRaw(f)
		}
	}
	return nil
}

// Send sends an ASDU on every started connection (spontaneous data, end
// of initialisation).
func (s *Station) Send(a *iec60870.ASDU) error {
	if a.CA == 0 {
		a.CA = s.CA
	}
	b, err := a.Encode(s.Params, s.Loc)
	if err != nil {
		return err
	}
	s.mu.Lock()
	var cs []*conn
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	var sent bool
	for _, c := range cs {
		if c.sendI(b) == nil {
			sent = true
		}
	}
	if !sent {
		return errors.New("no started connection")
	}
	return nil
}

// Connection states of Figures 17 and 36.
const (
	stStopped int32 = iota
	stStarted
	stPendingStop // STOPDT act received, own I-frames still unacknowledged
)

type conn struct {
	s      *Station
	se     *station.Session
	c      net.Conn
	secCh  chan []sec.Frame
	sec    *sec.SecureLink // the station's A-Profile when the connection was accepted
	wmu    sync.Mutex
	vs, vr uint16
	ack    uint16 // oldest own I-frame not yet acknowledged
	state  atomic.Int32
}

func (s *Station) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		cn := &conn{s: s, c: c, sec: s.Security}
		cn.se = s.st.Attach(station.SessionConfig{Params: s.Params, Loc: s.Loc, MaxASDU: cs104.MaxASDU,
			Send: func(_ context.Context, b []byte) error { return cn.sendI(b) }})
		// Under the IEC 62351-3 profile a timed key update on an idle
		// connection is preceded by a TESTFR act, as cs104.Link does.
		iotsec.SetKeepalive(c, func() error {
			cn.wmu.Lock()
			defer cn.wmu.Unlock()
			return cn.write(cs104.APDU{Format: cs104.FormatU, U: cs104.UTestFRAct})
		})
		if s.Security != nil {
			cn.secCh = make(chan []sec.Frame, 64)
		}
		s.conns[cn] = true
		s.accepts++
		s.mu.Unlock()
		go cn.serve()
	}
}

func (c *conn) write(a cs104.APDU) error {
	b, err := a.Encode()
	if err != nil {
		return err
	}
	_, err = c.c.Write(b)
	return err
}

// sendI sends an application ASDU, as Secure Data with the A-Profile (nil
// frames: queued until the session is established).
func (c *conn) sendI(asdu []byte) error {
	if sec := c.sec; sec != nil {
		frames, err := sec.Wrap(asdu)
		if err != nil {
			return err
		}
		c.s.mu.Lock()
		if len(frames) > 0 {
			c.s.lastSecure = frames
			if c.s.TamperNext {
				c.s.TamperNext = false
				last := append([]byte(nil), frames[len(frames)-1]...)
				last[len(last)-1] ^= 0x01
				frames = append(append([][]byte(nil), frames[:len(frames)-1]...), last)
			}
		}
		c.s.mu.Unlock()
		for _, f := range frames {
			if err := c.sendRaw(f); err != nil {
				return err
			}
		}
		return nil
	}
	return c.sendRaw(asdu)
}

// secSender sends the A-Profile's own messages and released Secure Data.
func (c *conn) secSender(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case frames := <-c.secCh:
			for _, f := range frames {
				_ = c.sendRaw(f.Data)
			}
		}
	}
}

func (c *conn) sendRaw(asdu []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.state.Load() != stStarted {
		return errors.New("not started")
	}
	err := c.write(cs104.APDU{Format: cs104.FormatI, SendSeq: c.vs, RecvSeq: c.vr, ASDU: asdu})
	c.vs = (c.vs + 1) % (1 << 15)
	return err
}

func (c *conn) serve() {
	s := c.s
	done := make(chan struct{})
	if c.sec != nil {
		go c.secSender(done)
	}
	defer func() {
		close(done)
		c.se.Close()
		if c.sec != nil {
			c.sec.Detach(c)
		}
		_ = c.c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	r := bufio.NewReader(c.c)
	for {
		a, err := cs104.ReadAPDU(r)
		if err != nil {
			return
		}
		switch a.Format {
		case cs104.FormatU:
			c.wmu.Lock()
			switch a.U {
			case cs104.UStartDTAct:
				s.mu.Lock()
				s.startDT++
				hold := s.NoStartDTCon
				s.mu.Unlock()
				if !hold && c.state.Load() == stStopped {
					// Figure 36: close every other connection not stopped.
					s.mu.Lock()
					for o := range s.conns {
						if o != c && o.state.Load() != stStopped {
							_ = o.c.Close()
						}
					}
					s.mu.Unlock()
					c.state.Store(stStarted)
					_ = c.write(cs104.APDU{Format: cs104.FormatU, U: cs104.UStartDTCon})
					if c.sec != nil {
						ch := c.secCh
						c.sec.Attach(c, func(f []sec.Frame) {
							select {
							case ch <- f:
							default:
							}
						})
					}
				}
			case cs104.UStopDTAct:
				if c.state.Load() == stStarted {
					if c.vs != c.ack {
						c.state.Store(stPendingStop)
					} else {
						c.state.Store(stStopped)
						_ = c.write(cs104.APDU{Format: cs104.FormatU, U: cs104.UStopDTCon})
					}
				}
			case cs104.UTestFRAct:
				s.mu.Lock()
				s.testFR++
				s.mu.Unlock()
				_ = c.write(cs104.APDU{Format: cs104.FormatU, U: cs104.UTestFRCon})
			}
			c.wmu.Unlock()
		case cs104.FormatS:
			s.mu.Lock()
			s.sRecv++
			s.mu.Unlock()
			c.wmu.Lock()
			st := c.state.Load()
			if st == stStopped {
				c.wmu.Unlock()
				return // Figure 17: S-frame while stopped, active close
			}
			c.ack = a.RecvSeq
			if st == stPendingStop && c.vs == c.ack {
				c.state.Store(stStopped)
				_ = c.write(cs104.APDU{Format: cs104.FormatU, U: cs104.UStopDTCon})
			}
			c.wmu.Unlock()
		case cs104.FormatI:
			s.mu.Lock()
			s.iRecv++
			noAck := s.NoAck
			s.mu.Unlock()
			c.wmu.Lock()
			if c.state.Load() != stStarted || a.SendSeq != c.vr {
				c.wmu.Unlock()
				return // Figure 17: I-frame while not started, or a gap
			}
			c.vr = (c.vr + 1) % (1 << 15)
			c.ack = a.RecvSeq
			if noAck {
				c.wmu.Unlock()
				continue
			}
			_ = c.write(cs104.APDU{Format: cs104.FormatS, RecvSeq: c.vr})
			c.wmu.Unlock()
			if c.sec != nil {
				data, ok := c.sec.Unwrap(a.ASDU)
				if !ok {
					continue
				}
				a.ASDU = data
			}
			if as, err := iec60870.DecodeASDU(a.ASDU, s.Params, s.Loc); err == nil {
				c.se.HandleASDU(as)
			} else {
				c.se.Handle(a.ASDU)
			}
		}
	}
}

func mirror(a *iec60870.ASDU, cause iec60870.Cause, negative bool) *iec60870.ASDU {
	m := *a
	m.Cause, m.Negative = cause, negative
	m.Objects = append([]iec60870.Object(nil), a.Objects...)
	return &m
}

// intercept sees every request before the library station: it records
// it, refuses other common addresses, runs Procedures, and applies the
// switches the library has no hook for (silent and late commands, a test
// command answered with another counter). The rest the library answers.
func (s *Station) intercept(a *iec60870.ASDU, reply func(*iec60870.ASDU)) bool {
	var first iec60870.Object
	if len(a.Objects) > 0 {
		first = a.Objects[0]
	}
	s.mu.Lock()
	s.requests = append(s.requests, Command{Type: a.Type, IOA: first.IOA, Cause: a.Cause})
	ca := s.CA
	s.mu.Unlock()
	if a.CA != ca && a.CA != 0xffff {
		reply(mirror(a, iec60870.CauseUnknownCA, true))
		return true
	}
	// The broadcast address is answered as the station's own.
	a.CA = ca
	sec := s.st.SectorAt(ca)
	var ioas []uint32
	for _, p := range sec.Points() {
		ioas = append(ioas, p.IOA)
	}
	get := func(ioa uint32) (iec60870.TypeID, float64, uint8, bool) {
		p, ok := sec.Get(ioa)
		return p.Type, p.Value, p.Quality, ok
	}
	if s.Proc.Handle(a, ioas, get, reply) {
		return true
	}
	switch a.Type {
	case iec60870.CIcNa1:
		s.mu.Lock()
		s.gi++
		s.mu.Unlock()
	case iec60870.CCiNa1:
		s.mu.Lock()
		s.ci++
		s.mu.Unlock()
	case iec60870.CCsNa1:
		s.mu.Lock()
		s.clock = append(s.clock, first.Time)
		s.mu.Unlock()
	case iec60870.CTsTa1:
		s.mu.Lock()
		mismatch := s.TestMismatch
		s.mu.Unlock()
		if mismatch && first.IOA == 0 {
			m := mirror(a, iec60870.CauseActCon, false)
			m.Objects[0].Value++
			reply(m)
			return true
		}
	case iec60870.CScNa1, iec60870.CDcNa1, iec60870.CRcNa1, iec60870.CSeNa1, iec60870.CSeNb1, iec60870.CSeNc1, iec60870.CBoNa1,
		iec60870.CScTa1, iec60870.CDcTa1, iec60870.CRcTa1, iec60870.CSeTa1, iec60870.CSeTb1, iec60870.CSeTc1, iec60870.CBoTa1:
		s.mu.Lock()
		late := a.Type.TimeTagged() && s.MaxDelay > 0 && time.Since(first.Time) > s.MaxDelay
		s.commands = append(s.commands, Command{Type: a.Type, IOA: first.IOA, Value: first.Value, Select: first.Select, Cause: a.Cause, Late: late})
		silent := s.SilentCommands
		s.mu.Unlock()
		return late || silent
	}
	return false
}
