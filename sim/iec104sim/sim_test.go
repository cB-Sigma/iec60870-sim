// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package iec104sim_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/cB-Sigma/iec60870"
	"github.com/cB-Sigma/iec60870-sim/sim/iec104sim"
	"github.com/cB-Sigma/iec60870/cs104"
	"github.com/cB-Sigma/iec60870/sec"
	"github.com/cB-Sigma/iotsec"
)

// data collects what a client received.
type data struct {
	mu   sync.Mutex
	objs map[uint32]iec60870.Object
	why  map[uint32]iec60870.Cause
}

func (d *data) on(a *iec60870.ASDU) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, o := range a.Objects {
		d.objs[o.IOA], d.why[o.IOA] = o, a.Cause
	}
}

func (d *data) get(ioa uint32) (iec60870.Object, iec60870.Cause, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.objs[ioa]
	return o, d.why[ioa], ok
}

func station(t *testing.T) *iec104sim.Station {
	t.Helper()
	s := iec104sim.New(1)
	s.Set(1001, iec104sim.Point{Type: iec60870.MDpNa1, Value: 1})
	s.Set(3001, iec104sim.Point{Type: iec60870.MMeNc1, Value: 230.5})
	s.Controls[2001] = 1001
	return s
}

func client(t *testing.T, addr string, cfg cs104.LinkConfig) (*cs104.Client, *data) {
	t.Helper()
	l, err := cs104.Dial(context.Background(), addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	d := &data{objs: map[uint32]iec60870.Object{}, why: map[uint32]iec60870.Cause{}}
	return cs104.NewClient(l, cs104.ClientConfig{CA: 1, Timeout: 2 * time.Second, OnData: d.on}), d
}

// The simulator answers a station interrogation from its database and a
// select-before-operate command with confirmation, return information
// and termination.
func TestSimInterrogationAndCommand(t *testing.T) {
	s := station(t)
	if err := s.Listen("", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	c, d := client(t, s.Addr(), cs104.LinkConfig{})
	ctx := context.Background()
	if err := c.Interrogate(ctx, iec60870.QOIStation, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if o, why, ok := d.get(3001); !ok || o.Value != float32Of(230.5) || why != iec60870.CauseInterrogated {
		t.Errorf("voltage %+v cause %s", o, why)
	}
	s.Lock()
	s.RequireSelect = true
	s.Unlock()
	if _, err := c.Operate(ctx, iec60870.Operation{Type: iec60870.CDcNa1, Object: iec60870.Object{IOA: 2001, Value: 2},
		SelectBeforeOperate: true, Termination: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if o, why, _ := d.get(1001); o.Value != 2 || why != iec60870.CauseReturnRemote {
		t.Errorf("breaker %+v cause %s after the command", o, why)
	}
	cmds := s.Commands()
	if len(cmds) != 2 || !cmds[0].Select || cmds[1].Select {
		t.Errorf("commands %+v", cmds)
	}
	// An execute without its select is refused by a station that
	// requires one.
	if _, err := c.Operate(ctx, iec60870.Operation{Type: iec60870.CDcNa1, Object: iec60870.Object{IOA: 2001, Value: 1}}); err == nil {
		t.Error("a direct execute was accepted")
	}
	// No activation termination: confirmed, outcome unknown.
	s.Lock()
	s.RequireSelect, s.NoActTerm = false, true
	s.Unlock()
	_, err := c.Operate(ctx, iec60870.Operation{Type: iec60870.CDcNa1, Object: iec60870.Object{IOA: 2001, Value: 1},
		Termination: 100 * time.Millisecond})
	if !iec60870.NotTerminated(err) {
		t.Errorf("without termination: %v", err)
	}
	if err := c.ClockSync(ctx, time.Now()); err != nil || len(s.ClockSyncs()) != 1 {
		t.Errorf("clock synchronisation: %v, %d received", err, len(s.ClockSyncs()))
	}
}

// float32Of is what a short float survives the wire as.
func float32Of(v float64) float64 { return float64(float32(v)) }

// The simulator serves under TLS (crypto/tls through iotsec.StdTLS); a
// client that does not trust it is refused.
func TestSimTLS(t *testing.T) {
	cert, pool := selfSigned(t, "rtu.test")
	s := station(t)
	s.TLS = iotsec.StdTLS{Config: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	if err := s.Listen("", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	c, d := client(t, s.Addr(), cs104.LinkConfig{TLS: iotsec.StdTLS{Config: &tls.Config{RootCAs: pool, ServerName: "rtu.test"}}})
	if err := c.Interrogate(context.Background(), iec60870.QOIStation, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := d.get(1001); !ok {
		t.Error("no data over TLS")
	}
	if _, err := cs104.Dial(context.Background(), s.Addr(), cs104.LinkConfig{T0: time.Second,
		TLS: iotsec.StdTLS{Config: &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "rtu.test"}}}); err == nil {
		t.Error("an untrusted server was accepted")
	}
}

// The A-Profile end to end on IEC 104: Station Association with pinned
// self-signed certificates, Session Key Change, then every ASDU as Secure
// Data.
func TestSimSecureAuthentication(t *testing.T) {
	scada, err := sec.NewIdentity(sec.Secp256r1, "scada", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rtu, err := sec.NewIdentity(sec.Secp256r1, "rtu", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := station(t)
	s.Security, err = sec.SecureConfig(sec.Config{Link: sec.TCP, Params: sec.Params104, MaxASDU: cs104.MaxASDU, CA: 1, AIS: 7,
		Key: rtu.Key, Certificate: rtu.Certificate, Trust: scada.Pin()}, sec.NewEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen("", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	sl, err := sec.SecureConfig(sec.Config{Controlling: true, Link: sec.TCP, Params: sec.Params104, MaxASDU: cs104.MaxASDU, CA: 1,
		AIM: 5, Key: scada.Key, Certificate: scada.Certificate, Trust: rtu.Pin()}, sec.NewEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sl.Run(ctx)
	c, d := client(t, s.Addr(), cs104.LinkConfig{Security: sl})
	deadline := time.Now().Add(5 * time.Second)
	for !sl.Endpoint().Status().SessionKeysValid {
		if time.Now().After(deadline) {
			t.Fatalf("no session keys: %+v", sl.Endpoint().Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := c.Interrogate(ctx, iec60870.QOIStation, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := d.get(3001); !ok {
		t.Error("no data under the A-Profile")
	}
	st := sl.Endpoint().Stats()
	if st.Get(sec.StatSuccessfulAssociations) != 1 || st.Get(sec.StatSuccessfulDataAuthentications) == 0 {
		t.Errorf("statistics %v", st.Snapshot())
	}
}

func selfSigned(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, pool
}
