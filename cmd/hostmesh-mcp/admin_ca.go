package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/authority"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/localfile"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type csrInspect struct {
	CSR    string `json:"csr_pem" jsonschema:"Entire public PEM CERTIFICATE REQUEST"`
	Lookup bool   `json:"lookup_registry,omitempty" jsonschema:"Also query the bound CA registry. Omit on a joining peer without a CA."`
}
type caEnroll struct {
	CSR     string            `json:"csr_pem" jsonschema:"Public PEM request received from the joining peer"`
	Digest  string            `json:"expected_csr_sha256" jsonschema:"Independently approved 64-hex SHA256 of DER CSR from the owner"`
	Profile authority.Profile `json:"profile" jsonschema:"Operator-approved identity and names. node_id must be a canonical lowercase UUID v4."`
}
type caRevoke struct {
	Cert string `json:"certificate_pem,omitempty" jsonschema:"Issued public PEM leaf or chain identifying the compromised key"`
	Node string `json:"node_id,omitempty" jsonschema:"Alternatively revoke the currently registered key for this node UUID"`
}
type installCert struct {
	Key   string `json:"key_file" jsonschema:"Path to the original node.key on this joining peer"`
	Dir   string `json:"directory" jsonschema:"New directory for the PUBLIC certificate files; distinct from the existing key directory"`
	Chain string `json:"certificate_pem" jsonschema:"Issued PEM leaf followed by intermediates"`
	Roots string `json:"ca_pem" jsonschema:"Public trust roots accepted from the operator"`
}

func (a *adminServer) addCertificates(s *mcp.Server) {
	type bootstrap struct {
		ServerName string `json:"server_name" jsonschema:"DNS name or IP in the EST renewal URL, without port or scheme"`
	}
	adminTool(s, "ca_bootstrap", "Create a fresh CA and initialized registry for the bound .../online/ca.json. Use once on the CA host. Keep offline/root.key offline.", false, false, func(_ context.Context, in bootstrap) (adminResult, error) {
		online := filepath.Dir(a.caPath)
		if filepath.Base(online) != "online" || filepath.Base(a.caPath) != "ca.json" {
			return nil, errors.New("bootstrap needs -ca-config <new-directory>/online/ca.json; use ca_init with an existing authority")
		}
		if err := authority.Bootstrap(filepath.Dir(online), in.ServerName); err != nil {
			return nil, err
		}
		return adminResult{"ca_config": a.caPath, "root_certificate_file": filepath.Join(online, "root.pem")}, nil
	})
	adminTool(s, "ca_init", "Create an empty registry for existing CA credentials. ca_bootstrap already initializes it.", false, false, func(context.Context, noArgs) (adminResult, error) {
		cfg, err := authority.ReadConfig(a.caPath)
		if err != nil {
			return nil, err
		}
		if err = authority.Init(cfg.StateDir); err != nil {
			return nil, err
		}
		return adminResult{"initialized": true}, nil
	})
	type csrDir struct {
		Dir string `json:"directory" jsonschema:"New private directory on the joining peer; its parent must already exist"`
	}
	adminTool(s, "peer_csr", "Create a joining peer's key locally. Send only csr_pem to the CA and confirm csr_sha256 over a trusted channel. Keep key_file on this peer.", false, false, func(_ context.Context, in csrDir) (adminResult, error) {
		if err := createPeerCSR(in.Dir); err != nil {
			return nil, err
		}
		dir, err := filepath.Abs(in.Dir)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "node.csr")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		req, err := authority.ParseEnrollmentRequest(data)
		if err != nil {
			return nil, err
		}
		return adminResult{"csr_file": path, "csr_pem": string(data), "csr_sha256": req.CSRSHA256, "key_file": filepath.Join(dir, "node.key")}, nil
	})
	adminTool(s, "ca_inspect_csr", "Read and verify a CSR. With lookup_registry, unregistered keys have no node_id. To decline a new request stop after inspection.", true, false, func(_ context.Context, in csrInspect) (adminResult, error) {
		req, err := authority.ParseEnrollmentRequest([]byte(in.CSR))
		if err != nil {
			return nil, err
		}
		out := adminResult{"csr_sha256": req.CSRSHA256, "public_key_sha256": req.PublicKeySHA256, "public_key_algorithm": req.Algorithm}
		if in.Lookup {
			cfg, err := authority.ReadConfig(a.caPath)
			if err != nil {
				return nil, err
			}
			state, id, err := authority.KeyStatus(cfg.StateDir, req.PublicKeySHA256)
			if err != nil {
				return nil, err
			}
			out["registry_status"] = state
			if id != "" {
				out["node_id"] = id
			}
		}
		return out, nil
	})
	adminTool(s, "ca_enroll", "Issue and register one approved CSR on the CA host. Use peer_install_certificate on the joining peer with the returned public chain and roots.", false, true, a.enrollCertificate)
	adminTool(s, "ca_revoke", "Disable future issuance for an enrolled key. Supply exactly one of certificate_pem or node_id. Existing certificates remain valid until expiry unless peers deny the node.", false, true, func(_ context.Context, in caRevoke) (adminResult, error) {
		if (in.Cert == "") == (in.Node == "") {
			return nil, errors.New("supply exactly one of certificate_pem or node_id")
		}
		cfg, err := authority.ReadConfig(a.caPath)
		if err != nil {
			return nil, err
		}
		var cert []byte
		if in.Cert != "" {
			cert = []byte(in.Cert)
		}
		if err = authority.Revoke(cfg.StateDir, in.Node, cert); err != nil {
			return nil, err
		}
		return adminResult{"revoked": true}, nil
	})
	adminTool(s, "peer_install_certificate", "Install public certificates in a fresh directory and verify the chain and local key. Use the three returned paths in the peer config.", false, false, installPeerCertificate)
}

// Re-encode public certificates so comments, headers and unrelated PEM material
// can never be copied into an MCP reply or a peer certificate file.
func publicBundle(data []byte) (string, error) {
	var out bytes.Buffer
	for len(bytes.TrimSpace(data)) > 0 {
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return "", errors.New("expected PEM certificates")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return "", err
		}
		if err := pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}); err != nil {
			return "", err
		}
		data = rest
	}
	if out.Len() == 0 {
		return "", errors.New("empty certificate bundle")
	}
	return out.String(), nil
}
func (a *adminServer) enrollCertificate(_ context.Context, in caEnroll) (adminResult, error) {
	req, err := authority.ParseEnrollmentRequest([]byte(in.CSR))
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(req.CSRSHA256, in.Digest) {
		return nil, errors.New("CSR_MISMATCH: expected_csr_sha256 must match the independently approved DER fingerprint")
	}
	ca, err := a.loadCA()
	if err != nil {
		return nil, err
	}
	chain, err := ca.Enroll(in.Profile, req)
	if err != nil {
		return nil, err
	}
	return adminResult{"certificate_pem": string(chain), "ca_pem": ca.PublicRoots(), "node_id": in.Profile.NodeID, "csr_sha256": req.CSRSHA256}, nil
}

func installPeerCertificate(_ context.Context, in installCert) (adminResult, error) {
	if in.Dir == "" || in.Key == "" {
		return nil, errors.New("directory and key_file required")
	}
	dir, err := filepath.Abs(in.Dir)
	if err != nil {
		return nil, err
	}
	key, err := filepath.Abs(in.Key)
	if err != nil {
		return nil, err
	}
	certPath, caPath := filepath.Join(dir, "node.pem"), filepath.Join(dir, "root.pem")
	chain, err := publicBundle([]byte(in.Chain))
	if err != nil {
		return nil, err
	}
	roots, err := publicBundle([]byte(in.Roots))
	if err != nil {
		return nil, err
	}
	var id string
	err = localfile.PopulatePrivateDir(dir, func() error {
		if err := os.WriteFile(certPath, []byte(chain), 0600); err != nil {
			return err
		}
		if err := os.WriteFile(caPath, []byte(roots), 0600); err != nil {
			return err
		}
		p, err := peer.LoadPeerTLS(caPath, certPath, key)
		if err != nil {
			return err
		}
		id = p.NodeID()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return adminResult{"node_id": id, "ca_file": caPath, "cert_file": certPath, "key_file": key}, nil
}
