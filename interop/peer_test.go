// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package interop

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// image is the reference peers' image, built from ./peer.
const image = "sigma-iec60870-interop:lib60870-2.4.1-p2"

// needDocker skips the test unless SIGMA_IEC60870_INTEROP=1 is set and
// Docker answers, and builds the peer image when it is missing.
func needDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("SIGMA_IEC60870_INTEROP") != "1" {
		t.Skip("set SIGMA_IEC60870_INTEROP=1 to run the interop tests (needs Docker)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not running")
	}
	if exec.Command("docker", "image", "inspect", image).Run() == nil {
		return
	}
	out, err := exec.Command("docker", "build", "-q", "-t", image, "peer").CombinedOutput()
	if err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
}

// freePort returns a TCP port free on the loopback interface.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// peer is a reference program running in a container: its standard
// input takes commands, its output lines are collected.
type peer struct {
	t     *testing.T
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string

	mu  sync.Mutex
	log []string
}

// startPeer runs `docker run --rm -i --name <unique> <args...>`.
func startPeer(t *testing.T, args ...string) *peer {
	t.Helper()
	p := &peer{t: t, name: fmt.Sprintf("sigma-iec60870-%d", time.Now().UnixNano()), lines: make(chan string, 4096)}
	full := append([]string{"run", "--rm", "-i", "--name", p.name}, args...)
	p.cmd = exec.Command("docker", full...)
	var err error
	if p.stdin, err = p.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	p.cmd.Stdout, p.cmd.Stderr = w, w
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			p.mu.Lock()
			p.log = append(p.log, line)
			p.mu.Unlock()
			select {
			case p.lines <- strings.TrimPrefix(line, "PEER "):
			default:
			}
		}
	}()
	go func() { _ = p.cmd.Wait(); _ = w.Close() }()
	t.Cleanup(func() {
		_, _ = io.WriteString(p.stdin, "quit\n")
		_ = p.stdin.Close()
		_ = exec.Command("docker", "stop", "-t", "2", p.name).Run()
		if t.Failed() || os.Getenv("SIGMA_IEC60870_INTEROP_LOG") == "1" {
			t.Logf("peer output:\n%s", p.output())
		}
	})
	return p
}

func (p *peer) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.log, "\n")
}

// send writes one command line to the peer.
func (p *peer) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.stdin, line+"\n"); err != nil {
		p.t.Fatalf("peer stdin: %v", err)
	}
}

// expect waits for an output line ("PEER " removed) containing all the
// parts, skipping others, and returns it.
func (p *peer) expect(within time.Duration, parts ...string) string {
	p.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case l := <-p.lines:
			ok := true
			for _, part := range parts {
				if !strings.Contains(l, part) {
					ok = false
					break
				}
			}
			if ok {
				return l
			}
		case <-deadline:
			p.t.Fatalf("no peer line with %q within %s", parts, within)
			return ""
		}
	}
}

// expectAny waits for a line containing one of alts.
func (p *peer) expectAny(within time.Duration, alts ...string) string {
	p.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case l := <-p.lines:
			for _, a := range alts {
				if strings.Contains(l, a) {
					return l
				}
			}
		case <-deadline:
			p.t.Fatalf("no peer line with one of %q within %s", alts, within)
			return ""
		}
	}
}

// kv splits a peer line into its key=value fields.
func kv(line string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			m[k] = v
		}
	}
	return m
}

// drain discards the lines received so far.
func (p *peer) drain() {
	for {
		select {
		case <-p.lines:
		default:
			return
		}
	}
}

// pki is a test CA with a certificate for the peer (written to dir as
// ca.pem, peer.pem and peer.key for the container) and one for us.
type pki struct {
	dir  string
	pool *x509.CertPool
	ours tls.Certificate
	// rogue is our certificate from an untrusted CA, roguePool trusts only
	// that CA, rogueDir holds peer files with a rogue peer certificate.
	rogue     tls.Certificate
	roguePool *x509.CertPool
	rogueDir  string
}

func newPKI(t *testing.T) pki {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "interop test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leaf := func(serial int64, name string) ([]byte, *ecdsa.PrivateKey) {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			DNSNames:    []string{name, "localhost", "host.docker.internal"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, k
	}
	write := func(name, typ string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ca.pem", "CERTIFICATE", caDER)
	peerDER, peerKey := leaf(2, "peer")
	write("peer.pem", "CERTIFICATE", peerDER)
	kb, _ := x509.MarshalECPrivateKey(peerKey)
	write("peer.key", "EC PRIVATE KEY", kb)
	ourDER, ourKey := leaf(3, "sigma")
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	k := pki{dir: dir, pool: pool, ours: tls.Certificate{Certificate: [][]byte{ourDER}, PrivateKey: ourKey}}

	// A second CA nobody trusts: a certificate of ours it signed, a pool
	// with only it, and rogueDir with the peer's files where the peer's
	// own certificate is the rogue one (its CA file stays the good one).
	rk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rTmpl := &x509.Certificate{SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: "rogue CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rDER, err := x509.CreateCertificate(rand.Reader, rTmpl, rTmpl, &rk.PublicKey, rk)
	if err != nil {
		t.Fatal(err)
	}
	rca, _ := x509.ParseCertificate(rDER)
	signed := func(serial int64, name string) ([]byte, *ecdsa.PrivateKey) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			DNSNames:    []string{name, "localhost", "host.docker.internal"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, rca, &key.PublicKey, rk)
		if err != nil {
			t.Fatal(err)
		}
		return der, key
	}
	roDER, roKey := signed(11, "sigma")
	k.rogue = tls.Certificate{Certificate: [][]byte{roDER}, PrivateKey: roKey}
	k.roguePool = x509.NewCertPool()
	k.roguePool.AddCert(rca)
	k.rogueDir = t.TempDir()
	rpDER, rpKey := signed(12, "peer")
	write2 := func(name, typ string, b []byte) {
		if err := os.WriteFile(filepath.Join(k.rogueDir, name), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write2("ca.pem", "CERTIFICATE", caDER)
	write2("peer.pem", "CERTIFICATE", rpDER)
	rkb, _ := x509.MarshalECPrivateKey(rpKey)
	write2("peer.key", "EC PRIVATE KEY", rkb)
	return k
}
