package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The test executable is the shebang interpreter of the signed faulty candidate.
// This also works in a scratch container with no shell or utilities.
func TestFaultCandidate(t *testing.T) {
	if os.Getenv("HOSTMESH_MCP_SUPERVISE_FIXTURE") != "1" || len(os.Args) < 4 {
		return
	}
	switch os.Args[3] {
	case "version":
		fmt.Println("99.0.0")
	case "config":
	case "serve":
		if err := os.WriteFile("ledger/state.json", []byte("corrupt"), 0600); err != nil {
			os.Exit(41)
		}
		if err := os.WriteFile("candidate.pid", []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(42)
		}
		if _, err := os.Stat("interrupt"); err == nil {
			syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		}
		os.Exit(43)
	default:
		os.Exit(44)
	}
	os.Exit(0)
}

func TestSupervisedUpdate(t *testing.T) {
	if os.Getenv("HOSTMESH_MCP_SUPERVISE_TEST") != "1" {
		t.Skip("opt-in standalone supervisor integration")
	}
	old, supervisor := os.Getenv("HOSTMESH_MCP_PREVIOUS_BINARY"), os.Getenv("HOSTMESH_MCP_TEST_BINARY")
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON := func(path string, v any) {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		write(path, data, 0600)
	}
	version := func(binary string) string {
		t.Helper()
		v, err := commandOutput(t.Context(), "", binary, "version")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	vOld, vNext := version(old), version(supervisor)
	work := t.TempDir()
	f := testReleaseFeed(t)
	var requests atomic.Int64
	var stall atomic.Bool
	waiting := make(chan struct{})
	files := http.FileServer(http.Dir(f.dir))
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/peer" && stall.CompareAndSwap(true, false) {
			close(waiting)
			<-r.Context().Done()
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(feed.Close)
	c := f.config
	c.ManifestURL = feed.URL + "/release.json"
	c.Unit, c.UserService = "", false
	c.Binary, c.PeerConfig = filepath.Join(work, "peer"), filepath.Join(work, "peer.json")
	settings, logPath := filepath.Join(work, "updater.json"), filepath.Join(work, "log")
	c.CheckInterval = "1h"
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	faulty := []byte("#!" + testExe + " -test.run=^TestFaultCandidate$\n")
	write(c.Binary, read(old), 0700)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	writeJSON(c.PeerConfig, config{StateDir: "ledger", MCPListen: addr})
	writeJSON(settings, c)
	target := updateTarget{Endpoint: "http://" + addr + "/mcp"}
	wait := func(what string, ok func() bool) {
		t.Helper()
		until := time.Now().Add(15 * time.Second)
		for !ok() {
			if time.Now().After(until) {
				t.Fatalf("waiting for %s; supervisor log:\n%s", what, read(logPath))
			}
			time.Sleep(40 * time.Millisecond)
		}
	}
	var cmd *exec.Cmd
	start := func() {
		t.Helper()
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer logFile.Close()
		cmd = exec.Command(supervisor, "supervise", "-config", settings)
		cmd.Stdout, cmd.Stderr = logFile, logFile
		cmd.Env = append(os.Environ(), "HOSTMESH_MCP_SUPERVISE_FIXTURE=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if cmd != nil && cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	checkedVersion := func(v string) bool { return target.probe(t.Context(), v) == nil }
	noJournal := func() bool { _, err := os.Lstat(c.Binary + ".update-pending"); return errors.Is(err, os.ErrNotExist) }
	pid := func() int {
		t.Helper()
		// /proc/.../children is per thread. Find the one that owns serve.
		files, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			b, _ := os.ReadFile(f)
			for _, p := range strings.Fields(string(b)) {
				args, _ := os.ReadFile("/proc/" + p + "/cmdline")
				if strings.HasPrefix(string(args), c.Binary+"\x00serve\x00") {
					n, _ := strconv.Atoi(p)
					return n
				}
			}
		}
		return 0
	}
	check := func() {
		t.Helper()
		if err := cmd.Process.Signal(syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
	}
	stop := func(sig syscall.Signal) {
		t.Helper()
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		if sig != syscall.SIGKILL && err != nil {
			t.Fatal(err)
		}
	}
	// Boot while the feed has no release, then recover a killed serve process.
	start()
	wait("old identity", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, "ledger", "state.json"))
		var state struct {
			ID string `json:"self_id"`
		}
		if json.Unmarshal(b, &state) != nil || state.ID == "" {
			return false
		}
		target.NodeID = state.ID
		return checkedVersion(vOld)
	})
	if requests.Load() != 0 {
		t.Fatal("boot contacted the feed")
	}
	f.publish(t, "99.0.0", faulty)
	stall.Store(true)
	check()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("download did not start")
	}
	first := pid()
	if first == 0 {
		t.Fatal("missing child")
	}
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	wait("peer respawn", func() bool { return pid() != 0 && pid() != first && checkedVersion(vOld) })
	if _, err := commandOutput(t.Context(), "", supervisor, "supervise", "-config", settings); err == nil || !strings.Contains(err.Error(), "update-lock") {
		t.Fatalf("duplicate supervisor: %v", err)
	}
	if _, err := commandOutput(t.Context(), "", supervisor, "update", "apply", "-config", settings); err == nil || !strings.Contains(err.Error(), "SIGUSR1") {
		t.Fatalf("independent apply: %v", err)
	}
	// A corrupt signature must leave the existing process running.
	f.publish(t, "99.0.0", faulty)
	write(filepath.Join(f.dir, "release.json.sig"), make([]byte, 64), 0600)
	before := pid()
	check()
	wait("signature rejection", func() bool { return strings.Contains(string(read(logPath)), "invalid release manifest signature") })
	if pid() != before || !checkedVersion(vOld) {
		t.Fatal("signature rejection affected running peer")
	}
	// A signed candidate passes preflight but damages the ledger on startup.
	f.publish(t, "99.0.0", faulty)
	check()
	wait("rollback", func() bool {
		return strings.Contains(string(read(logPath)), "restored "+vOld) && noJournal() && checkedVersion(vOld)
	})
	if err := os.Remove(filepath.Join(work, "candidate.pid")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(work, "interrupt"), nil, 0600)
	check()
	pidPath := filepath.Join(work, "candidate.pid")
	wait("candidate before acceptance", func() bool { b, _ := os.ReadFile(pidPath); return len(b) > 0 && !noJournal() })
	failedPID := strings.TrimSpace(string(read(pidPath)))
	stop(syscall.SIGKILL)
	wait("orphan termination", func() bool {
		stat, err := os.ReadFile("/proc/" + failedPID + "/stat")
		return errors.Is(err, os.ErrNotExist) || strings.Contains(string(stat), ") Z ")
	})
	// With the release files removed, restart must recover the old binary and ledger.
	if err := os.Remove(filepath.Join(f.dir, "release.json")); err != nil {
		t.Fatal(err)
	}
	// A different peer configuration cannot take over this recovery journal.
	wrong := c
	wrong.PeerConfig = filepath.Join(work, "other.json")
	writeJSON(settings, wrong)
	if _, err := commandOutput(t.Context(), "", supervisor, "supervise", "-config", settings); err == nil || !strings.Contains(err.Error(), "different service") {
		t.Fatalf("wrong recovery target: %v", err)
	}
	countBeforeRecovery := requests.Load()
	c.CheckInterval = "2s"
	writeJSON(settings, c)
	start()
	wait("offline recovery", func() bool { return noJournal() && checkedVersion(vOld) })
	if requests.Load() != countBeforeRecovery {
		t.Fatal("recovery contacted the feed")
	}
	f.publish(t, vNext, read(supervisor))
	wait("scheduled update", func() bool { return noJournal() && checkedVersion(vNext) })
	before = pid()
	seen := strings.Count(string(read(logPath)), "Installed "+vNext+", published "+vNext)
	check()
	wait("same-version check", func() bool {
		return strings.Count(string(read(logPath)), "Installed "+vNext+", published "+vNext) > seen
	})
	if before == 0 || pid() != before {
		t.Fatal("same release restarted child")
	}
	stop(syscall.SIGTERM)
	if err := syscall.Kill(before, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child still exists after graceful stop: %v", err)
	}
}
