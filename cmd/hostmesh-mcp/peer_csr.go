package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/localfile"
)

func runPeer(args []string) error {
	const usage = "usage: hostmesh-mcp peer csr -dir <new-directory>"
	if len(args) == 0 || args[0] != "csr" {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet("peer csr", flag.ContinueOnError)
	dir := flags.String("dir", "", "new directory for the private node.key and enrollment request node.csr")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *dir == "" || flags.NArg() != 0 {
		return errors.New(usage)
	}
	if err := createPeerCSR(*dir); err != nil {
		return err
	}
	fmt.Printf("Send this CSR to the CA administrator: %s\nKeep this private key on the peer: %s\n", filepath.Join(*dir, "node.csr"), filepath.Join(*dir, "node.key"))
	return nil
}

func createPeerCSR(dir string) error {
	if dir == "" {
		return errors.New("new credentials directory is required")
	}
	return localfile.PopulatePrivateDir(dir, func() error { return writePeerCSR(dir) })
}

func writePeerCSR(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	// The CA assigns the node ID, SANs and seeds from its approved profile.
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "node.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "node.csr"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0600)
}
