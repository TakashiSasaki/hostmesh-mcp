package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/peer"
	"github.com/TakashiSasaki/host-status-mcp/internal/state"
	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	wire "github.com/TakashiSasaki/host-status-mcp/proto/hoststatus/peer/v1"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
)

type testCA struct {
	cert *x509.Certificate
	key  ed25519.PrivateKey
	path string
}

func writeTestPEM(t *testing.T, kind string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func newTestCA(t *testing.T) testCA {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &x509.Certificate{Subject: pkix.Name{CommonName: "test CA"}, SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, c, c, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{c, key, writeTestPEM(t, "CERTIFICATE", der)}
}

func (ca testCA) issue(t *testing.T, id string, until time.Time, names ...string) peerConfig {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse("urn:host-status-mcp:node:" + id)
	if err != nil {
		t.Fatal(err)
	}
	c := &x509.Certificate{SerialNumber: serial, URIs: []*url.URL{uri}, DNSNames: names, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: until}
	der, err := x509.CreateCertificate(rand.Reader, c, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return peerConfig{CAFile: ca.path, CertFile: writeTestPEM(t, "CERTIFICATE", der), KeyFile: writeTestPEM(t, "PRIVATE KEY", secret)}
}

func TestMTLSAdmission(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	until := time.Now().Add(time.Hour)
	denied := uuid.NewString()
	cfg := ca.issue(t, uuid.NewString(), until)
	cfg.Listen = "127.0.0.1:0"
	cfg.DenyNodes = []string{denied}
	server := startServeProcess(t, config{Peer: cfg})
	id := uuid.NewString()
	valid := ca.issue(t, id, until)
	outsider := other.issue(t, id, until)
	expired := ca.issue(t, id, time.Now().Add(-time.Minute))
	deniedCert := ca.issue(t, denied, until)
	roots := x509.NewCertPool()
	data, err := os.ReadFile(ca.path)
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(data)
	for _, tc := range []struct {
		name        string
		client      peerConfig
		claim, host string
	}{
		{"missing-client", peerConfig{}, id, "127.0.0.1"},
		{"foreign-client", outsider, id, "127.0.0.1"},
		{"expired-client", expired, id, "127.0.0.1"},
		{"wrong-server-name", valid, id, "incorrect.test"},
		{"wrong-hello-id", valid, uuid.NewString(), "127.0.0.1"},
		{"denied", deniedCert, denied, "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tlsCfg := &tls.Config{RootCAs: roots, ServerName: tc.host, MinVersion: tls.VersionTLS13}
			if tc.client.CertFile != "" {
				pair, err := tls.LoadX509KeyPair(tc.client.CertFile, tc.client.KeyFile)
				if err != nil {
					t.Fatal(err)
				}
				tlsCfg.Certificates = []tls.Certificate{pair}
			}
			conn, err := grpc.NewClient("passthrough:///"+server.peer, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			link, err := wire.NewPeeringClient(conn).Link(ctx)
			if err != nil {
				return
			}
			// The signed record is valid for the certificate holder; altering the Hello ID must fail.
			record := &wire.NodeRecord{Id: tc.claim, Revision: 1, Hostname: "test", Os: "linux"}
			if tc.client.CertFile != "" && tc.client.CertFile != expired.CertFile {
				sec, err := peer.LoadPeerTLS(tc.client.CAFile, tc.client.CertFile, tc.client.KeyFile)
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "wrong-hello-id" {
					different := ca.issue(t, tc.claim, until)
					sec, err = peer.LoadPeerTLS(different.CAFile, different.CertFile, different.KeyFile)
					if err != nil {
						t.Fatal(err)
					}
				}
				proof, err := sec.Sign(status.Node{ID: sec.NodeID(), Hostname: "test", OS: "linux", Revision: 1})
				if err != nil {
					t.Fatal(err)
				}
				record.CertChain, record.Signature = proof.Chain, proof.Signature
			}
			if err := link.Send(&wire.Frame{Body: &wire.Frame_Hello{Hello: &wire.Hello{Version: 1, LinkId: uuid.NewString(), Node: record}}}); err != nil {
				return
			}
			if frame, err := link.Recv(); err == nil {
				t.Fatalf("unexpected admission frame: %v", frame)
			}
		})
	}
}

func TestMTLSRecordTrust(t *testing.T) {
	ca := newTestCA(t)
	dir := t.TempDir()
	local := status.Node{Hostname: "A", OS: "linux"}
	plain, err := state.Open(dir, local)
	if err != nil {
		t.Fatal(err)
	}
	id := plain.Self().ID
	n := status.Node{ID: uuid.NewString(), Revision: 1, Hostname: "B", OS: "linux"}
	if _, _, err := plain.Merge([]status.Node{n}); err != nil {
		t.Fatal(err)
	}
	plain.Close()
	load := func(id string) *peer.PeerTLS {
		c := ca.issue(t, id, time.Now().Add(time.Hour))
		tls, err := peer.LoadPeerTLS(c.CAFile, c.CertFile, c.KeyFile)
		if err != nil {
			t.Fatal(err)
		}
		return tls
	}
	a, b := load(id), load(n.ID)
	if wrong, err := state.OpenWithTrust(dir, local, b); err == nil {
		wrong.Close()
		t.Fatal("mismatched local identity accepted")
	}
	secure, err := state.OpenWithTrust(dir, local, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(secure.Nodes()) != 1 {
		t.Fatal("imported plaintext directory")
	}
	if _, _, err := secure.Merge([]status.Node{n}); err == nil {
		t.Fatal("unsigned record accepted")
	}
	n.Proof, err = b.Sign(n)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := secure.Merge([]status.Node{n}); err != nil {
		t.Fatal(err)
	}
	n.Revision++
	if _, _, err := secure.Merge([]status.Node{n}); err == nil {
		t.Fatal("modified record accepted")
	}
	secure.Close()
	standalone, err := state.OpenLocal(dir, local)
	if err != nil {
		t.Fatal(err)
	}
	standalone.Close()
	secure, err = state.OpenWithTrust(dir, local, load(id))
	if err != nil {
		t.Fatal(err)
	}
	if len(secure.Nodes()) != 2 || secure.Self().Revision <= 2 {
		t.Fatal("signed directory or renewed revision missing")
	}
	signed := secure.Nodes()
	secure.Close()
	plain, err = state.Open(dir, local)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if len(plain.Nodes()) != 1 || plain.Self().ID != id || plain.Self().Proof != nil {
		t.Fatal("plaintext restart retained authenticated records")
	}
	if _, _, err := plain.Merge(signed); err == nil || len(plain.Nodes()) != 1 {
		t.Fatal("plaintext ledger imported signed records")
	}
}

func testClientCredentials(t *testing.T, cfg peerConfig) credentials.TransportCredentials {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	pemData, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pemData)
	return credentials.NewTLS(&tls.Config{RootCAs: roots, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13})
}

func TestMTLSLinkExpiry(t *testing.T) {
	ca := newTestCA(t)
	hub := ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
	hub.Listen = "127.0.0.1:0"
	server := startServeProcess(t, config{Peer: hub})
	id := uuid.NewString()
	deadline := time.Now().UTC().Truncate(time.Second).Add(4 * time.Second)
	cfg := ca.issue(t, id, deadline)
	security, err := peer.LoadPeerTLS(cfg.CAFile, cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient("passthrough:///"+server.peer, grpc.WithTransportCredentials(testClientCredentials(t, cfg)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithDeadline(t.Context(), deadline.Add(3*time.Second))
	defer cancel()
	link, err := wire.NewPeeringClient(conn).Link(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := status.Node{ID: id, Revision: 1, Hostname: "short-lived", OS: "linux"}
	proof, err := security.Sign(n)
	if err != nil {
		t.Fatal(err)
	}
	record := &wire.NodeRecord{Id: id, Revision: 1, Hostname: n.Hostname, Os: n.OS, CertChain: proof.Chain, Signature: proof.Signature}
	err = link.Send(&wire.Frame{Body: &wire.Frame_Hello{Hello: &wire.Hello{Version: 1, LinkId: uuid.NewString(), Node: record}}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := link.Recv()
	if err != nil || msg.GetHello() == nil {
		t.Fatalf("valid link: %v", err)
	}
	for err == nil {
		_, err = link.Recv()
	}
	if ctx.Err() != nil || time.Now().Before(deadline) {
		t.Fatalf("wrong disconnect time: %v", err)
	}
}

// Each edge seeds both hubs. The MCP clients on all four nodes must see
// and reach only the two nodes sharing their transport.
func TestPeerModeIsolation(t *testing.T) {
	ca := newTestCA(t)
	hub := func(cfg peerConfig) readyProcess {
		cfg.Listen = "127.0.0.1:0"
		return startServeProcess(t, config{Peer: cfg, MCPListen: "127.0.0.1:0"})
	}
	secureConfig := func() peerConfig { return ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour)) }
	plainHub, secureHub := hub(peerConfig{}), hub(secureConfig())
	// Probe both listeners synchronously so a slow background seed attempt
	// cannot make an incompatible listener look isolated.
	for _, probe := range []struct {
		server readyProcess
		creds  credentials.TransportCredentials
	}{
		{secureHub, insecure.NewCredentials()},
		{plainHub, testClientCredentials(t, secureConfig())},
	} {
		conn, err := grpc.NewClient("passthrough:///"+probe.server.peer, grpc.WithTransportCredentials(probe.creds))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		_, err = wire.NewPeeringClient(conn).Link(ctx)
		cancel()
		conn.Close()
		if grpcstatus.Code(err) != codes.Unavailable {
			t.Fatalf("mixed-mode transport to %s: expected handshake failure, got %v", probe.server.peer, err)
		}
	}
	edge := func(cfg peerConfig, foreign, own readyProcess) readyProcess {
		cfg.Seeds = []string{foreign.peer, own.peer}
		return startServeProcess(t, config{Peer: cfg, MCPListen: "127.0.0.1:0"})
	}
	plainEdge := edge(peerConfig{}, secureHub, plainHub)
	secureEdge := edge(secureConfig(), plainHub, secureHub)
	for _, pair := range [][2]readyProcess{{plainHub, plainEdge}, {plainEdge, plainHub}, {secureHub, secureEdge}, {secureEdge, secureHub}} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		client := mcp.NewClient(&mcp.Implementation{Name: "tls-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: pair[0].mcp}, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		for {
			reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_status", Arguments: map[string]any{"target": map[string]string{"node_id": pair[1].id}, "sections": []string{"system"}}})
			if err != nil {
				session.Close()
				cancel()
				t.Fatal(err)
			}
			if !reply.IsError {
				data, err := json.Marshal(reply.StructuredContent)
				var got status.Result
				if err != nil || json.Unmarshal(data, &got) != nil || got.Node.ID != pair[1].id || got.Route.Kind != "direct" || got.Sections.System == nil {
					t.Fatalf("unexpected direct reply %s", data)
				}
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_list"})
		if err != nil || reply.IsError {
			t.Fatalf("host_list: %v, %v", reply, err)
		}
		var listed struct {
			Hosts []status.Host `json:"hosts"`
		}
		data, err := json.Marshal(reply.StructuredContent)
		if err != nil || json.Unmarshal(data, &listed) != nil || len(listed.Hosts) != 2 {
			t.Fatalf("unexpected directory on %s: %s", pair[0].id, data)
		}
		for _, h := range listed.Hosts {
			if h.ID != pair[0].id && h.ID != pair[1].id {
				t.Fatalf("foreign node %s in directory of %s", h.ID, pair[0].id)
			}
		}
		foreign := plainHub
		if pair[0] == plainHub || pair[0] == plainEdge {
			foreign = secureHub
		}
		reply, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "host_status", Arguments: map[string]any{"target": map[string]string{"node_id": foreign.id}, "sections": []string{"system"}}})
		if err != nil || !reply.IsError || len(reply.Content) != 1 {
			t.Fatalf("foreign node lookup: %v, %v", reply, err)
		}
		content, ok := reply.Content[0].(*mcp.TextContent)
		var failure status.Failure
		if !ok || json.Unmarshal([]byte(content.Text), &failure) != nil || failure.Code != "HOST_NOT_FOUND" {
			t.Fatalf("unexpected foreign node lookup error: %v", reply.Content)
		}
		session.Close()
		cancel()
	}
}
