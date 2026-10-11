// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package cs104_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cB-Sigma/iec60870/cs104"
	"github.com/cB-Sigma/iotsec"
)

// fakeTLS passes the connection through, records the server name it was
// asked for, and wraps it in a connection that takes a keepalive.
type fakeTLS struct {
	name string
	mu   sync.Mutex
	got  []string
	conn *kaConn
}

func (f *fakeTLS) ServerName() string { return f.name }

func (f *fakeTLS) Client(_ context.Context, raw net.Conn, serverName string) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, serverName)
	f.conn = &kaConn{Conn: raw}
	return f.conn, nil
}

type kaConn struct {
	net.Conn
	mu sync.Mutex
	ka iotsec.Keepalive
}

func (c *kaConn) SetKeepalive(f iotsec.Keepalive) { c.mu.Lock(); c.ka = f; c.mu.Unlock() }

// Dial and DialStandby use the configured dialer; a refusing one fails
// the connection.
func TestLinkUsesDialer(t *testing.T) {
	s := startSim(t)
	var calls int
	var mu sync.Mutex
	d := iotsec.DialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		var nd net.Dialer
		return nd.DialContext(ctx, network, addr)
	})
	l, err := cs104.Dial(context.Background(), s.Addr(), cs104.LinkConfig{Dialer: d})
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if calls != 1 {
		t.Errorf("dialer called %d times", calls)
	}
	refuse := iotsec.DialerFunc(func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("egress refused") })
	if _, err := cs104.DialStandby(context.Background(), s.Addr(), cs104.LinkConfig{Dialer: refuse}); err == nil {
		t.Error("a refused dial connected")
	}
}

// The certificate is checked against the TLS client's own name, else the
// host the caller dialled; the link registers its TESTFR as the
// connection's keepalive (IEC 62351-3 timed key updates on idle links).
func TestLinkTLSNameAndKeepalive(t *testing.T) {
	s := startSim(t)
	_, port, _ := net.SplitHostPort(s.Addr())
	f := &fakeTLS{}
	l, err := cs104.Dial(context.Background(), net.JoinHostPort("localhost", port), cs104.LinkConfig{TLS: f, T3: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f.name = "rtu.example"
	l2, err := cs104.DialStandby(context.Background(), s.Addr(), cs104.LinkConfig{TLS: f, T3: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	f.mu.Lock()
	got := append([]string(nil), f.got...)
	f.mu.Unlock()
	if len(got) != 2 || got[0] != "localhost" || got[1] != "rtu.example" {
		t.Errorf("server names %q", got)
	}
	f.conn.mu.Lock()
	ka := f.conn.ka
	f.conn.mu.Unlock()
	if ka == nil {
		t.Fatal("no keepalive registered")
	}
	if err := ka(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, "the TESTFR act", func() bool { return l2.Stats().TestFRSent >= 1 })
}
