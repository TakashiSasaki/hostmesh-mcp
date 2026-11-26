package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentHelp(t *testing.T) {
	for topic, description := range helpTopics {
		args := strings.Fields(topic)
		for _, form := range [][]string{append([]string{"help"}, args...), append(args, "--help")} {
			cmd := appCommand(t.Context(), form...)
			out, err := cmd.Output()
			if err != nil || string(out) != description+"\n" {
				t.Fatalf("%v: %v %s", form, err, out)
			}
		}
	}
	if err := appCommand(t.Context(), "help", "wrong").Run(); err == nil {
		t.Fatal("unknown topic accepted")
	}
}

func TestAgentEnrollment(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	caCmd(t, true, "bootstrap", "-dir", ca, "-server-name", "ca.example.internal")
	online := filepath.Join(ca, "online")
	cfg := filepath.Join(online, "ca.json")
	registry := filepath.Join(online, "state", "registry.json")
	initial := readFile(t, registry)
	peerDir := filepath.Join(dir, "peer")
	if err := appCommand(t.Context(), "peer", "csr", "-dir", peerDir).Run(); err != nil {
		t.Fatal(err)
	}
	csr := filepath.Join(peerDir, "node.csr")
	raw, _ := pem.Decode(readFile(t, csr))
	sum := sha256.Sum256(raw.Bytes)
	digest := hex.EncodeToString(sum[:])
	id := "7043f65e-b1f6-489d-a0d2-5d402035735d"
	profile := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(profile, []byte(`{"node_id":"`+id+`","dns_names":["sensor.example.internal"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(dir, "issued.pem")
	inspect := func(withRegistry bool) map[string]string {
		t.Helper()
		args := []string{"inspect", "-csr", csr, "-json"}
		if withRegistry {
			args = append(args, "-config", cfg)
		}
		var result map[string]string
		if err := json.Unmarshal(caCmd(t, true, args...), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if v := inspect(false); v["registry_status"] != "" || v["csr_sha256"] != digest {
		t.Fatal(v)
	}
	if v := inspect(true); v["registry_status"] != "unregistered" || v["public_key_sha256"] == digest {
		t.Fatal(v)
	}
	errorCode := func(code string, args ...string) {
		t.Helper()
		cmd := appCommand(t.Context(), append([]string{"ca"}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		var problem struct{ Code string }
		if err == nil || len(out) != 0 || json.Unmarshal(stderr.Bytes(), &problem) != nil || problem.Code != code {
			t.Fatalf("%v: err=%v out=%s stderr=%s", args, err, out, &stderr)
		}
	}
	enroll := []string{"enroll", "-config", cfg, "-profile", profile, "-csr", csr, "-json", "-out", chain, "-expect-csr-sha256"}
	errorCode("CSR_MISMATCH", append(enroll, strings.Repeat("0", 64))...)
	errorCode("INVALID_ARGUMENT", append(enroll, "")...)
	invalid := filepath.Join(dir, "invalid.csr")
	raw.Bytes[len(raw.Bytes)-1] ^= 1
	if err := os.WriteFile(invalid, pem.EncodeToMemory(raw), 0600); err != nil {
		t.Fatal(err)
	}
	errorCode("INVALID_CSR", "inspect", "-csr", invalid, "-json")
	if _, err := os.Stat(chain); !os.IsNotExist(err) || !bytes.Equal(initial, readFile(t, registry)) {
		t.Fatal("inspection or unsuccessful approval changed state")
	}
	var receipt map[string]string
	if err := json.Unmarshal(caCmd(t, true, append(enroll, strings.ToUpper(digest))...), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["node_id"] != id || receipt["certificate_file"] != chain || receipt["csr_sha256"] != digest {
		t.Fatal(receipt)
	}
	issued := readFile(t, chain)
	if err := mustLeaf(t, issued).VerifyHostname("sensor.example.internal"); err != nil {
		t.Fatal(err)
	}
	if v := inspect(true); v["registry_status"] != "active" || v["node_id"] != id {
		t.Fatal(v)
	}
	errorCode("OUTPUT_EXISTS", append(enroll, digest)...)
	if !bytes.Equal(issued, readFile(t, chain)) {
		t.Fatal("existing certificate changed")
	}
	caCmd(t, true, "revoke", "-config", cfg, "-cert", chain)
	if v := inspect(true); v["registry_status"] != "revoked" {
		t.Fatal(v)
	}
	another := filepath.Join(dir, "another.pem")
	errorCode("KEY_REVOKED", "enroll", "-json", "-out", another, "-config", cfg, "-csr", csr, "-profile", profile)
	if _, err := os.Stat(another); !os.IsNotExist(err) {
		t.Fatal("failed enrollment left a certificate file")
	}
}
