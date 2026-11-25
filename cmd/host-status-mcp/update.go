package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func runUpdate(ctx context.Context, args []string) error {
	const usage = "usage: host-status-mcp update {check|apply} -config <updater.json>"
	if len(args) == 0 || (args[0] != "check" && args[0] != "apply") {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("host-status-mcp update "+args[0], flag.ContinueOnError)
	configPath := fs.String("config", "", "local updater configuration file")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *configPath == "" || fs.NArg() != 0 {
		return errors.New(usage)
	}
	c, err := readUpdateSettings(*configPath)
	if err != nil {
		return err
	}
	pending := c.Binary + ".update-pending"
	if args[0] == "apply" {
		lock, err := lockUpdateFile(c.Binary+".update-lock", os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	if _, err := os.Lstat(pending); err == nil {
		if args[0] == "check" {
			return errors.New("unfinished update: use update apply to recover")
		}
		if err := restoreUpdate(ctx, c, pending); err != nil {
			return fmt.Errorf("recover interrupted update: %w", err)
		}
		fmt.Println("Recovered previous service; retry apply to check for a release")
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	info, err := os.Lstat(c.Binary)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("binary must be a regular standalone executable: %s", c.Binary)
	}
	current, err := commandOutput(ctx, "", c.Binary, "version")
	if err != nil {
		return err
	}
	version, artifact, err := loadRelease(ctx, c)
	if err != nil {
		return err
	}
	newer, err := newerRelease(current, version)
	if err != nil {
		return err
	}
	fmt.Printf("Installed %s, published %s\n", current, version)
	if !newer || args[0] == "check" {
		return nil
	}
	return applyRelease(ctx, c, pending, current, version, artifact, info.Mode().Perm())
}

func lockUpdateFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Copy via a sibling temporary file: the backup's presence means the whole
// ledger was saved. The updater and peer run under the same account.
func copyLedger(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".ledger-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func applyRelease(ctx context.Context, c updateSettings, pending, current, version string, a releaseArtifact, mode os.FileMode) error {
	parent := filepath.Dir(c.Binary)
	stage, err := os.MkdirTemp(parent, ".host-status-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	candidate := filepath.Join(stage, "candidate")
	if err := stageRelease(ctx, candidate, a, mode); err != nil {
		return err
	}
	got, err := commandOutput(ctx, "", candidate, "version")
	if err != nil {
		return err
	}
	if got != version {
		return fmt.Errorf("candidate reports %q, manifest says %q", got, version)
	}
	target, err := inspectTarget(ctx, c, current, candidate)
	if err != nil {
		return err
	}
	// Publish the complete recovery directory before touching the running service.
	if err := os.Link(c.Binary, filepath.Join(stage, "previous")); err != nil {
		return err
	}
	record, err := json.Marshal(target)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stage, "target.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(record)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := syncDir(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, pending); err != nil {
		return err
	}
	if err := syncDir(parent); err != nil {
		return err
	}
	if err := activateUpdate(ctx, c.Binary, pending, version, target); err != nil {
		recovery, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if recoveryErr := restoreUpdate(recovery, c, pending); recoveryErr != nil {
			return errors.Join(err, fmt.Errorf("rollback incomplete (retry update apply): %w", recoveryErr))
		}
		return fmt.Errorf("update failed; restored %s: %w", current, err)
	}
	if err := finishUpdate(pending); err != nil {
		return err
	}
	fmt.Printf("Updated to %s; verified node %s\n", version, target.NodeID)
	return nil
}

func activateUpdate(ctx context.Context, binary, pending, version string, t updateTarget) error {
	if _, err := t.systemctl(ctx, "stop"); err != nil {
		return err
	}
	lock, err := lockUpdateFile(filepath.Join(t.StateDir, "state.lock"), os.O_RDWR)
	if err != nil {
		return err
	}
	err = copyLedger(filepath.Join(t.StateDir, "state.json"), filepath.Join(pending, "state.json"))
	if err == nil {
		err = os.Rename(filepath.Join(pending, "candidate"), binary)
	}
	if err == nil {
		err = syncDir(filepath.Dir(binary))
	}
	lock.Close()
	if err != nil {
		return err
	}
	return t.startAndVerify(ctx, binary, version)
}

func restoreUpdate(ctx context.Context, c updateSettings, pending string) error {
	raw, err := os.ReadFile(filepath.Join(pending, "target.json"))
	if err != nil {
		return err
	}
	var t updateTarget
	if err := decodeUpdateJSON(raw, &t); err != nil {
		return err
	}
	if t.OwnerUID != os.Geteuid() || t.Unit != c.Unit || t.User != c.UserService {
		return errors.New("pending update belongs to a different service")
	}
	if _, err := t.systemctl(ctx, "stop"); err != nil {
		return err
	}
	lock, err := lockUpdateFile(filepath.Join(t.StateDir, "state.lock"), os.O_RDWR)
	if err != nil {
		return err
	}
	err = restoreFiles(c.Binary, pending, t.StateDir)
	lock.Close()
	if err != nil {
		return err
	}
	// A stopped unit may already have been garbage-collected by systemd.
	// Drop any start-rate limit if present; start and the probe decide success.
	_, _ = t.systemctl(ctx, "reset-failed")
	if err := t.startAndVerify(ctx, c.Binary, t.Previous); err != nil {
		return err
	}
	return finishUpdate(pending)
}

func restoreFiles(binary, pending, stateDir string) error {
	// Keep previous intact until the old service answers; restoration is retryable.
	link := filepath.Join(pending, "restore")
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(filepath.Join(pending, "previous"), link); err != nil {
		return err
	}
	if err := os.Rename(link, binary); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(binary)); err != nil {
		return err
	}
	backup := filepath.Join(pending, "state.json")
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return copyLedger(backup, filepath.Join(stateDir, "state.json"))
}

func finishUpdate(pending string) error {
	// Remove the journal name atomically so a crash during cleanup cannot
	// leave a half-deleted transaction for the next invocation to recover.
	dir := filepath.Dir(pending)
	discard, err := os.MkdirTemp(dir, ".host-status-finished-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(discard)
	if err := os.Rename(pending, filepath.Join(discard, "transaction")); err != nil {
		return err
	}
	return syncDir(dir)
}
