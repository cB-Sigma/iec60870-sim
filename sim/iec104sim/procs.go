package iec104sim

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/cB-Sigma/iec60870"
)

// Procedures answers the IEC 60870-5-101 clause 7.4 procedures beyond
// station interrogation and commands: group interrogation, counter
// interrogation with freeze and reset, read, reset process, the test
// command without time tag, delay acquisition, parameter loading and
// activation, and file transfer. The IEC 104 and IEC 101 test stations
// share it.
type Procedures struct {
	mu sync.Mutex
	// Groups maps an IOA to its interrogation groups; CounterGroups an
	// integrated total to its counter group.
	Groups        map[uint32][]int
	CounterGroups map[uint32]int
	// Params are the parameters in operation, by IOA; KeepParams confirms
	// a parameter but keeps the old value; unknown IOAs are refused.
	Params     map[uint32]float64
	KeepParams bool
	// Cyclic are the objects whose cyclic transmission may be
	// (de)activated, and whether it is active.
	Cyclic map[uint32]bool
	// Files are the offered files, by IOA and name; received files land
	// in Received.
	Files    map[[2]uint32][]byte
	Received map[[2]uint32][]byte
	// Logs are log files by IOA and name: records with their time stamps,
	// answered by the query log (F_SC_NB_1) as lines of "RFC 3339 time,
	// data".
	Logs map[[2]uint32][]LogRecord
	// ReactionTime is how long the station takes before confirming a
	// delay acquisition.
	ReactionTime time.Duration

	fs       *iec60870.FileServer
	frozen   map[uint32]float64
	base     map[uint32]float64
	qccs     []uint8
	resets   []uint8
	delay    time.Duration
	delaySet bool
	reads    []uint32
	qois     []uint8
}

// LogRecord is one record of a log file.
type LogRecord struct {
	T    time.Time
	Data string
}

// NewProcedures creates the procedure state.
func NewProcedures() *Procedures {
	p := &Procedures{Groups: map[uint32][]int{}, CounterGroups: map[uint32]int{}, Params: map[uint32]float64{},
		Cyclic: map[uint32]bool{}, Files: map[[2]uint32][]byte{}, Received: map[[2]uint32][]byte{}, Logs: map[[2]uint32][]LogRecord{},
		frozen: map[uint32]float64{}, base: map[uint32]float64{}}
	p.fs = &iec60870.FileServer{Store: memFiles{p}, SectionSize: 300, SegmentSize: 100}
	return p
}

// Lock and Unlock guard the exported maps and switches.
func (p *Procedures) Lock() { p.mu.Lock() }

// Unlock releases Lock.
func (p *Procedures) Unlock() { p.mu.Unlock() }

// QCCs are the counter interrogation qualifiers received.
func (p *Procedures) QCCs() []uint8 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint8(nil), p.qccs...)
}

// Resets are the reset process qualifiers received.
func (p *Procedures) Resets() []uint8 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint8(nil), p.resets...)
}

// Delay is the transmission delay loaded by C_CD_NA_1 cause 3.
func (p *Procedures) Delay() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.delay, p.delaySet
}

// QOIs are the group interrogation qualifiers received.
func (p *Procedures) QOIs() []uint8 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint8(nil), p.qois...)
}

// CorruptSection makes the station send a wrong checksum for the given
// section once.
func (p *Procedures) CorruptSection(nos uint8) {
	once := true
	p.fs.CorruptChecksum = func(n uint8) bool {
		if n == nos && once {
			once = false
			return true
		}
		return false
	}
}

// Reads are the IOAs read with C_RD_NA_1.
func (p *Procedures) Reads() []uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint32(nil), p.reads...)
}

type memFiles struct{ p *Procedures }

func (m memFiles) Directory(context.Context) ([]iec60870.DirEntry, error) {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	var out []iec60870.DirEntry
	for k, b := range m.p.Files {
		out = append(out, iec60870.DirEntry{IOA: k[0], Name: uint16(k[1]), Length: uint32(len(b)),
			Time: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IOA < out[j].IOA })
	return out, nil
}

func (m memFiles) Read(_ context.Context, ioa uint32, name uint16) ([]byte, error) {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	b, ok := m.p.Files[[2]uint32{ioa, uint32(name)}]
	if !ok {
		return nil, iec60870.ErrNoFile
	}
	return b, nil
}

func (m memFiles) ReadLog(_ context.Context, ioa uint32, name uint16, start, stop time.Time) ([]byte, error) {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	recs, ok := m.p.Logs[[2]uint32{ioa, uint32(name)}]
	if !ok {
		return nil, iec60870.ErrNoFile
	}
	var b []byte
	for _, r := range recs {
		if (start.IsZero() || !r.T.Before(start)) && (stop.IsZero() || !r.T.After(stop)) {
			b = append(b, r.T.UTC().Format(time.RFC3339Nano)+","+r.Data+"\n"...)
		}
	}
	return b, nil
}

func (m memFiles) Write(_ context.Context, ioa uint32, name uint16, data []byte) error {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	m.p.Received[[2]uint32{ioa, uint32(name)}] = append([]byte(nil), data...)
	return nil
}

// Lookup reads a point of the station: its type, value and quality.
type Lookup func(ioa uint32) (t iec60870.TypeID, v float64, q uint8, ok bool)

func mirrorOf(a *iec60870.ASDU, c iec60870.Cause, neg bool) *iec60870.ASDU {
	m := *a
	m.Cause, m.Negative = c, neg
	m.Objects = append([]iec60870.Object(nil), a.Objects...)
	return &m
}

// Handle answers a if it belongs to these procedures and reports whether
// it did. ioas lists the station's points; get reads one.
func (p *Procedures) Handle(a *iec60870.ASDU, ioas []uint32, get Lookup, reply func(*iec60870.ASDU)) bool {
	if a.Type.File() {
		return p.fs.Handle(context.Background(), a, reply)
	}
	var o iec60870.Object
	if len(a.Objects) > 0 {
		o = a.Objects[0]
	}
	sort.Slice(ioas, func(i, j int) bool { return ioas[i] < ioas[j] })
	switch a.Type {
	case iec60870.CIcNa1:
		q := uint8(o.Value)
		g := iec60870.InterrogationGroup(q)
		if g == 0 {
			return false // the station's own station interrogation
		}
		p.mu.Lock()
		p.qois = append(p.qois, q)
		p.mu.Unlock()
		reply(mirrorOf(a, iec60870.CauseActCon, false))
		p.mu.Lock()
		var objs []iec60870.Object
		var types []iec60870.TypeID
		for _, ioa := range ioas {
			for _, x := range p.Groups[ioa] {
				if x == g {
					t, v, qd, _ := get(ioa)
					types = append(types, t)
					objs = append(objs, iec60870.Object{IOA: ioa, Value: v, Quality: qd})
				}
			}
		}
		p.mu.Unlock()
		for i, ob := range objs {
			reply(&iec60870.ASDU{Type: types[i], Cause: iec60870.Cause(q), Objects: []iec60870.Object{ob}})
		}
		reply(mirrorOf(a, iec60870.CauseActTerm, false))
	case iec60870.CCiNa1:
		rqt, frz := iec60870.SplitQCC(uint8(o.Value))
		if rqt == iec60870.RQTGeneral && frz == iec60870.FRZRead {
			return false // the station's own general counter request
		}
		p.mu.Lock()
		p.qccs = append(p.qccs, uint8(o.Value))
		p.mu.Unlock()
		if rqt < 1 || rqt > iec60870.RQTGeneral {
			reply(mirrorOf(a, iec60870.CauseActCon, true))
			return true
		}
		in := func(ioa uint32) bool {
			t, _, _, _ := get(ioa)
			return (t == iec60870.MItNa1 || t == iec60870.MItTb1 || t == iec60870.MItTa1) && (rqt == iec60870.RQTGeneral || p.CounterGroups[ioa] == int(rqt))
		}
		reply(mirrorOf(a, iec60870.CauseActCon, false))
		p.mu.Lock()
		var out []*iec60870.ASDU
		for _, ioa := range ioas {
			if !in(ioa) {
				continue
			}
			t, v, qd, _ := get(ioa)
			switch frz {
			case iec60870.FRZRead:
				val := v - p.base[ioa]
				if f, ok := p.frozen[ioa]; ok {
					val = f
				}
				out = append(out, &iec60870.ASDU{Type: t, Cause: iec60870.CauseCounterGen + iec60870.Cause(rqt%5),
					Objects: []iec60870.Object{{IOA: ioa, Value: val, Quality: qd}}})
			case iec60870.FRZFreeze:
				p.frozen[ioa] = v - p.base[ioa]
			case iec60870.FRZFreezeReset:
				p.frozen[ioa] = v - p.base[ioa]
				p.base[ioa] = v
			case iec60870.FRZReset:
				p.base[ioa] = v
			}
		}
		p.mu.Unlock()
		for _, r := range out {
			reply(r)
		}
		if frz == iec60870.FRZRead {
			reply(mirrorOf(a, iec60870.CauseActTerm, false))
		}
	case iec60870.CRdNa1:
		p.mu.Lock()
		p.reads = append(p.reads, o.IOA)
		p.mu.Unlock()
		t, v, qd, ok := get(o.IOA)
		if !ok || t == iec60870.MItNa1 || t == iec60870.MItTb1 {
			reply(mirrorOf(a, iec60870.CauseUnknownObject, true))
			return true
		}
		reply(&iec60870.ASDU{Type: t, Cause: iec60870.CauseRequest, Objects: []iec60870.Object{{IOA: o.IOA, Value: v, Quality: qd}}})
	case iec60870.CRpNa1:
		p.mu.Lock()
		p.resets = append(p.resets, uint8(o.Value))
		p.mu.Unlock()
		q := uint8(o.Value)
		if q != iec60870.QRPGeneral && q != iec60870.QRPEvents {
			reply(mirrorOf(a, iec60870.CauseActCon, true))
			return true
		}
		reply(mirrorOf(a, iec60870.CauseActCon, false))
		if q == iec60870.QRPGeneral {
			reply(&iec60870.ASDU{Type: iec60870.MEiNa1, Cause: iec60870.CauseInitialised, Objects: []iec60870.Object{{Value: iec60870.COIRemote}}})
		}
	case iec60870.CTsNa1:
		// 7.3.4.5: the confirmation repeats the fixed test pattern.
		if o.IOA != 0 {
			reply(mirrorOf(a, iec60870.CauseUnknownObject, true))
			return true
		}
		reply(mirrorOf(a, iec60870.CauseActCon, o.Value != iec60870.FBP))
	case iec60870.CCdNa1:
		switch a.Cause {
		case iec60870.CauseActivation:
			// IEC 60870-5-5 6.13: an auxiliary clock set to SDT on
			// receipt; the confirmation carries SDT + tR.
			p.mu.Lock()
			rt := p.ReactionTime
			p.mu.Unlock()
			time.Sleep(rt)
			m := mirrorOf(a, iec60870.CauseActCon, false)
			m.Objects[0].Elapsed = time.Duration((o.Elapsed.Milliseconds()+rt.Milliseconds())%60000) * time.Millisecond
			reply(m)
		case iec60870.CauseSpontaneous:
			p.mu.Lock()
			p.delay, p.delaySet = o.Elapsed, true
			p.mu.Unlock()
		default:
			reply(mirrorOf(a, iec60870.CauseUnknownCause, true))
		}
	case iec60870.PMeNa1, iec60870.PMeNb1, iec60870.PMeNc1:
		p.mu.Lock()
		old, ok := p.Params[o.IOA]
		keep := p.KeepParams
		if ok && !keep {
			p.Params[o.IOA] = o.Value
		}
		p.mu.Unlock()
		if !ok {
			reply(mirrorOf(a, iec60870.CauseUnknownObject, true))
			return true
		}
		m := mirrorOf(a, iec60870.CauseActCon, false)
		if keep {
			m.Objects[0].Value = old
		}
		reply(m)
	case iec60870.PAcNa1:
		p.mu.Lock()
		_, ok := p.Cyclic[o.IOA]
		if ok && o.Qualifier == iec60870.QPACyclic {
			p.Cyclic[o.IOA] = a.Cause == iec60870.CauseActivation
		}
		p.mu.Unlock()
		c := iec60870.CauseActCon
		if a.Cause == iec60870.CauseDeactivation {
			c = iec60870.CauseDeactCon
		}
		reply(mirrorOf(a, c, !ok || o.Qualifier != iec60870.QPACyclic))
	default:
		return false
	}
	return true
}
