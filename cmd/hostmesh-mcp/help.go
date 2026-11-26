package main

import (
	"fmt"
	"strings"
)

// Topics are also accessible offline, on hosts with only the binary installed.
var helpTopics = map[string]string{
	"": `hostmesh-mcp — monitor hosts through MCP and connect peers in a mesh.

  admin              Discover and call administration tools from a shell
  mcp                Start the local administration MCP server over stdio
  stdio, serve       Start a local stdio or network MCP server
  config             Generate or validate peer settings
  peer               Generate a joining peer's key and enrollment request
  ca                 Inspect requests, issue certificates and operate a CA
  update, supervise  Manage Linux binary updates
  version            Print the executable version

Run hostmesh-mcp help <command> for syntax.
For administration workflows run hostmesh-mcp help administration.
For first-time certificate enrollment, run hostmesh-mcp help enrollment.`,
	"enrollment": `First enrollment (local CLI operations; ca serve does not need to be running)
   For a newly created CA, ca.json below means <bootstrap-directory>/online/ca.json.
1. Joining peer: hostmesh-mcp peer csr -dir mesh-peer
   Send node.csr to the operator. Keep node.key on the peer.
   Use hostmesh-mcp ca inspect -csr mesh-peer/node.csr to obtain csr_sha256.
   Confirm that fingerprint with the owner over a trusted channel.
2. CA operator: hostmesh-mcp ca inspect -csr received.csr -config ca.json
   This reads the request and registry. A valid signature proves key possession.
   An unregistered key has no node_id yet. The operator assigns it in profile.json.
   Approve the owner and profile separately. To decline, stop here. No pending
   registration exists. Revoke is for a key that was already registered.
3. Create profile.json from the authorized node identity and names, for example:
   {"node_id":"7043f65e-b1f6-489d-a0d2-5d402035735d","dns_names":["sensor.example.internal"]}
   Use a distinct canonical UUID v4 per node. Optional fields: dns_names,
   ip_addresses, seeds (host:port strings), client_only (boolean).
4. Issue on the CA host, substituting the independently approved digest:
   hostmesh-mcp ca enroll -config ca.json -profile profile.json -csr received.csr -expect-csr-sha256 <approved-digest> -out node-issued.pem -json
5. Return node-issued.pem and the public root.pem to the peer. Configure
   peer.cert_file, peer.ca_file and peer.key_file (the original node.key).
   Use absolute paths: peer paths resolve from the process working directory.
   hostmesh-mcp config check -config peer.json verifies the chain and key.`,
	"peer":     "hostmesh-mcp peer csr -dir <new-directory>\nSee hostmesh-mcp help enrollment for the complete workflow.",
	"peer csr": "hostmesh-mcp peer csr -dir <new-directory>\nCreates node.key and node.csr on the joining peer. Send node.csr to the CA operator. Keep node.key local. See hostmesh-mcp help enrollment.",
	"ca": `hostmesh-mcp ca <command>
  bootstrap      Create a new offline root and online issuing authority
  config         Generate or check CA settings
  init           Create an empty registry for an existing CA configuration
  inspect        Read and verify a CSR, optionally look up its key in the registry
  enroll         Issue a peer certificate and register its key
  revoke         Disable future issuance for a registered key
  serve          Start the EST renewal service
See hostmesh-mcp help enrollment for the first-enrollment workflow.`,
	"ca inspect": `hostmesh-mcp ca inspect -csr <request.pem> [-config <ca.json>] [-json]
Read-only. Verifies the signature and prints JSON with csr_sha256 (SHA-256 of
the DER request) and public_key_sha256 (SHA-256 of DER SubjectPublicKeyInfo).
-config adds registry_status (unregistered, active or revoked) and the registered
node_id for active/revoked keys. An unregistered key has no assigned node_id.
Without -config, the registry is not queried.
-json also formats errors as JSON on stderr. See hostmesh-mcp help enrollment.`,
	"ca enroll": `hostmesh-mcp ca enroll -config <ca.json> -profile <profile.json> -csr <request.pem> [-expect-csr-sha256 <hex>] [-out <new.pem>] [-json]
Writes the registry and issues a certificate using the approved profile.
-expect-csr-sha256 pins the DER request to the independently approved digest.
Default stdout is the PEM chain. -out creates a new file and refuses overwrite.
-json requires -out and prints a JSON receipt; failures go to stderr with nonzero exit.
See hostmesh-mcp help enrollment for the profile format and an example.`,
	"ca bootstrap":      "hostmesh-mcp ca bootstrap -dir <new-directory> -server-name <DNS-or-IP>\nGenerates offline/root.key and online/ca.json with an initialized registry.",
	"ca init":           "hostmesh-mcp ca init -config <ca.json>\nCreates an empty registry. A CA created by ca bootstrap already has one.",
	"ca serve":          "hostmesh-mcp ca serve -config <ca.json>\nStarts the EST renewal service on the configured loopback listener.",
	"ca revoke":         "hostmesh-mcp ca revoke -config <ca.json> {-cert <peer.pem> | -node-id <UUID>}\nRevokes an enrolled key. To decline an unregistered CSR, simply do not enroll it.",
	"ca config":         "hostmesh-mcp ca config example | hostmesh-mcp ca config check -config <ca.json>",
	"ca config example": "hostmesh-mcp ca config example\nPrint a CA configuration template as JSON.",
	"ca config check":   "hostmesh-mcp ca config check -config <ca.json>\nValidate the CA configuration, credentials and registry if present.",
	"config":            "hostmesh-mcp config example [-join] [-mtls] | hostmesh-mcp config check -config <peer.json>",
	"config example":    "hostmesh-mcp config example [-join] [-mtls]\nPrint a peer JSON template. -join dials an existing peer; -mtls includes the CA, certificate and key settings. Replace sample endpoints and paths.",
	"config check":      "hostmesh-mcp config check -config <peer.json>\nValidate peer settings and configured credentials.",
	"serve":             "hostmesh-mcp serve -config <peer.json>\nStart a peer. Generate a template with: hostmesh-mcp config example > peer.json",
	"stdio":             "hostmesh-mcp stdio [-state-dir <directory>]\nRun a local MCP server on standard input and output.",
	"supervise":         "hostmesh-mcp supervise -config <updater.json>\nSupervise and update a Linux peer.",
	"update":            "hostmesh-mcp update {check|apply} -config <updater.json> (Linux)",
	"update check":      "hostmesh-mcp update check -config <updater.json>\nCheck for a Linux peer update.",
	"update apply":      "hostmesh-mcp update apply -config <updater.json>\nApply a Linux systemd peer update. In supervise mode send SIGUSR1 to the supervisor.",
	"admin": `hostmesh-mcp admin [-peer-config path] [-ca-config path] [-update-config path] list | describe <tool> | call <tool> [JSON|-]
List tools first, then describe one for its required inputs. Pass - to read a JSON object from stdin.
Examples: hostmesh-mcp admin describe ca_enroll
          hostmesh-mcp admin call admin_info
          hostmesh-mcp admin call config_check '{"kind":"peer"}'
Long-running services use the native serve, ca serve and supervise commands. See help administration.`,
	"administration": `Local hostmesh administration
Discover: CLI: hostmesh-mcp admin list / admin describe <tool>. MCP: tools/list.
Read admin_info for the version, OS and bound paths. Supply -peer-config, -ca-config
and -update-config at startup (CLI: before list/describe/call). Use separate config files.
1. New CA only: ca_bootstrap creates the default mesh-ca/online/ca.json and registry.
2. Joining peer: peer_csr returns the public CSR, its digest and the local key path.
3. CA operator: ca_inspect_csr with lookup_registry=true verifies the request.
   An unregistered key has no node_id. Decline by stopping. To approve, ca_enroll
   requires the owner's independently confirmed CSR digest and a profile with a
   unique UUID v4. Return certificate_pem and ca_pem to the joining peer.
4. Joining peer: peer_install_certificate verifies the chain with the original key.
   Copy the three returned absolute paths into peer_config_example's peer object.
   mcp_listen is the separate TOP-LEVEL monitoring HTTP listener (default 127.0.0.1:8787).
   For tests use mcp_listen="127.0.0.1:0" and a unique state_dir. The peer listener
   is peer.listen. Omit fixed peer.advertise entries for a loopback-only test.
   Peer mTLS protects mesh traffic. The monitoring URL uses loopback HTTP.
   peer_start returns both the allocated MCP URL and peer address.
   Peer paths resolve from the process working directory. CA paths use ca.json's directory.
5. Edit the template then config_save with kind=peer and config=<edited object>.
   To edit an existing file first config_read and pass its sha256 as expected_sha256.
   config_check validates credentials and settings. CLI example:
   hostmesh-mcp admin call config_check '{"kind":"peer"}'
6. MCP: peer_start or ca_start, service_status, then service_stop. These services
   stop when the administration session exits. CLI: serve or ca serve -config <path>
   runs in the foreground until stopped. For an existing Linux systemd unit bind
   -peer-unit or -ca-unit at startup, then use unit_start/status/stop/restart.
7. Linux updates: update_check / update_apply for systemd. For a supervised peer
   use supervisor_start and supervisor_update (CLI: supervise and SIGUSR1).
8. Lost or compromised key: ca_revoke an issued certificate, then ca_inspect_csr
   with lookup_registry=true to confirm revoked. For replacement generate a new key.
For shell automation, pass - to admin call and pipe JSON on stdin. Save the complete
ca_enroll response as JSON to transfer its PEM strings without hand-copying them.
For a Linux updater template, call update_config_example.
For native CLI enrollment commands and a profile example: hostmesh-mcp help enrollment.`,
	"mcp":     "hostmesh-mcp mcp [-peer-config peer.json] [-ca-config mesh-ca/online/ca.json] [-update-config updater.json]\nLinux options: -peer-unit <name.service>, -ca-unit <name.service>, -user-units.\nLocal administrator tools over stdio. Paths are fixed at launch. Services started here stop when this session closes.",
	"version": "hostmesh-mcp version\nPrint the executable version.",
}

func commandHelp(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	var parts []string
	switch {
	case args[0] == "help":
		parts = args[1:]
	case args[len(args)-1] == "--help" || args[len(args)-1] == "-h":
		parts = args[:len(args)-1]
		// Only intercept a command path, never a flag argument.
		for _, part := range parts {
			if strings.HasPrefix(part, "-") {
				return false, nil
			}
		}
	default:
		return false, nil
	}
	topic := strings.Join(parts, " ")
	message, ok := helpTopics[topic]
	if !ok {
		return true, fmt.Errorf("unknown help topic %q (see hostmesh-mcp --help)", topic)
	}
	_, err := fmt.Println(message)
	return true, err
}
