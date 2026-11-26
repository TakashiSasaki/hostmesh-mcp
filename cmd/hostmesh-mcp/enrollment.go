package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/authority"
)

type enrollmentOptions struct {
	csr, config, profile, expected, output string
	json                                   bool
}

func runEnrollment(command string, args []string) error {
	var o enrollmentOptions
	fs := flag.NewFlagSet("ca "+command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.csr, "csr", "", "PEM certificate request")
	fs.StringVar(&o.config, "config", "", "CA JSON configuration")
	fs.BoolVar(&o.json, "json", false, "machine-readable output and errors")
	if command == "enroll" {
		fs.StringVar(&o.profile, "profile", "", "approved node profile JSON")
		fs.Func("expect-csr-sha256", "approved SHA-256 of DER request", func(s string) error {
			digest, err := hex.DecodeString(s)
			if err != nil || len(digest) != sha256.Size {
				return errors.New("expected 64 hex digits (SHA-256 of DER CSR)")
			}
			o.expected = strings.ToLower(s)
			return nil
		})
		fs.StringVar(&o.output, "out", "", "new file for issued PEM chain")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(helpTopics["ca "+command])
			return nil
		}
		// Parsing can fail before the -json switch. Always make parse errors machine-readable.
		return enrollmentOptions{json: true}.failure("INVALID_ARGUMENT", err)
	}
	if o.csr == "" || fs.NArg() != 0 || (command == "enroll" && (o.config == "" || o.profile == "" || (o.json && o.output == ""))) {
		return o.failure("INVALID_ARGUMENT", fmt.Errorf("see hostmesh-mcp help ca %s for required arguments", command))
	}
	data, err := os.ReadFile(o.csr)
	if err != nil {
		return o.failure("READ_CSR", err)
	}
	req, err := authority.ParseEnrollmentRequest(data)
	if err != nil {
		return o.failure("INVALID_CSR", err)
	}
	var cfg authority.Config
	if o.config != "" {
		cfg, err = authority.ReadConfig(o.config)
		if err != nil {
			return o.failure("INVALID_CONFIG", err)
		}
	}
	if command == "inspect" {
		result := struct {
			*authority.EnrollmentRequest
			Status string `json:"registry_status,omitempty"`
			NodeID string `json:"node_id,omitempty"`
		}{EnrollmentRequest: req}
		if o.config != "" {
			result.Status, result.NodeID, err = authority.KeyStatus(cfg.StateDir, req.PublicKeySHA256)
			if err != nil {
				return o.failure("READ_REGISTRY", err)
			}
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if o.expected != "" && req.CSRSHA256 != o.expected {
		return o.failure("CSR_MISMATCH", errors.New("CSR does not match approved SHA-256"))
	}
	return o.issue(cfg, req)
}

func (o enrollmentOptions) failure(code string, err error) error {
	if !o.json {
		return err
	}
	data, _ := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, err.Error()})
	return errors.New(string(data))
}

func (o enrollmentOptions) issue(cfg authority.Config, req *authority.EnrollmentRequest) error {
	p, err := authority.ReadProfile(o.profile)
	if err != nil {
		return o.failure("INVALID_PROFILE", err)
	}
	a, err := authority.Load(cfg)
	if err != nil {
		return o.failure("LOAD_CA", err)
	}
	var file *os.File
	if o.output != "" {
		file, err = os.OpenFile(o.output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			return o.failure("OUTPUT_EXISTS", err)
		}
		if err != nil {
			return o.failure("CREATE_OUTPUT", err)
		}
	}
	chain, err := a.Enroll(p, req)
	if err != nil {
		if file != nil {
			file.Close()
			os.Remove(o.output)
		}
		switch {
		case errors.Is(err, authority.ErrKeyRevoked):
			return o.failure("KEY_REVOKED", err)
		case errors.Is(err, authority.ErrNodeConflict):
			return o.failure("NODE_ID_CONFLICT", err)
		default:
			return o.failure("ENROLL_FAILED", err)
		}
	}
	if file == nil {
		_, err = os.Stdout.Write(chain)
	} else {
		if _, err = file.Write(chain); err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return o.failure("WRITE_ISSUED_CHAIN", fmt.Errorf("key already registered; writing issued certificate: %w", err))
	}
	if o.json {
		return json.NewEncoder(os.Stdout).Encode(struct {
			*authority.EnrollmentRequest
			NodeID string `json:"node_id"`
			File   string `json:"certificate_file"`
		}{req, p.NodeID, o.output})
	}
	return nil
}
