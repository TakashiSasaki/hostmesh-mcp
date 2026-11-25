//go:build !windows

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
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

func runUpdate(ctx context.Context, args []string) error {
	if runtime.GOOS != "linux" {
		return errors.New("binary updates require Linux")
	}
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
	manager := updateTarget{Unit: c.Unit, User: c.UserService}
	pending := c.Binary + ".update-pending"
	if args[0] == "apply" {
		if c.PeerConfig != "" {
			return errors.New("supervised peer: send SIGUSR1 to the supervisor to apply an update")
		}
		lock, err := lockUpdateFile(c.Binary+".update-lock", os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err := cleanUpdateWork(pending); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(pending); err == nil {
		if args[0] == "check" {
			return errors.New("unfinished update: restart supervise or run systemd update apply with the original settings")
		}
		if err := restoreUpdate(ctx, c, pending, manager); err != nil {
			return fmt.Errorf("recover interrupted update: %w", err)
		}
		fmt.Println("Recovered previous service; retry apply to check for a release")
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return updatePeer(ctx, ctx, c, manager, args[0] == "check")
}

// Preparation may be cancelled independently if the monitored child exits.
func updatePeer(ctx, prep context.Context, c updateSettings, manager peerController, checkOnly bool) error {
	info, err := os.Lstat(c.Binary)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("binary must be a regular standalone executable: %s", c.Binary)
	}
	current, err := commandOutput(prep, "", c.Binary, "version")
	if err != nil {
		return err
	}
	version, artifact, err := loadRelease(prep, c)
	if err != nil {
		return err
	}
	newer, err := newerRelease(current, version)
	if err != nil {
		return err
	}
	fmt.Printf("Installed %s, published %s\n", current, version)
	if !newer || checkOnly {
		return nil
	}
	return applyRelease(ctx, prep, c, manager, c.Binary+".update-pending", current, version, artifact, info.Mode().Perm())
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

func applyRelease(ctx, prep context.Context, c updateSettings, manager peerController, pending, current, version string, a releaseArtifact, mode os.FileMode) error {
	parent := filepath.Dir(c.Binary)
	stage := pending + ".stage"
	if err := os.Mkdir(stage, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	candidate := filepath.Join(stage, "candidate")
	if err := stageRelease(prep, candidate, a, mode); err != nil {
		return err
	}
	got, err := commandOutput(prep, "", candidate, "version")
	if err != nil {
		return err
	}
	if got != version {
		return fmt.Errorf("candidate reports %q, manifest says %q", got, version)
	}
	target, err := inspectTarget(prep, c, manager, current, candidate)
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
	if err := activateUpdate(ctx, c.Binary, pending, version, target, manager); err != nil {
		if c.PeerConfig != "" && ctx.Err() != nil {
			return ctx.Err() // Leave the durable journal for supervisor startup.
		}
		parent := context.Background()
		if c.PeerConfig != "" {
			parent = ctx
		}
		recovery, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		if recoveryErr := restoreUpdate(recovery, c, pending, manager); recoveryErr != nil {
			return errors.Join(err, fmt.Errorf("rollback incomplete: %w", recoveryErr))
		}
		return fmt.Errorf("update failed; restored %s: %w", current, err)
	}
	if err := finishUpdate(pending); err != nil {
		return err
	}
	fmt.Printf("Updated to %s; verified node %s\n", version, target.NodeID)
	return nil
}

func activateUpdate(ctx context.Context, binary, pending, version string, t updateTarget, manager peerController) error {
	if err := manager.stop(ctx); err != nil {
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
	return t.startAndVerify(ctx, manager, binary, version, false)
}

func recoveryTarget(c updateSettings, pending string) (updateTarget, error) {
	var t updateTarget
	raw, err := os.ReadFile(filepath.Join(pending, "target.json"))
	if err != nil {
		return t, err
	}
	if err := decodeUpdateJSON(raw, &t); err != nil {
		return t, err
	}
	if t.OwnerUID != os.Geteuid() || t.Unit != c.Unit || t.User != c.UserService || t.PeerConfig != c.PeerConfig {
		return t, errors.New("pending update belongs to a different service")
	}
	return t, nil
}

func restoreUpdate(ctx context.Context, c updateSettings, pending string, manager peerController) error {
	t, err := recoveryTarget(c, pending)
	if err != nil {
		return err
	}
	if err := manager.stop(ctx); err != nil {
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
	if err := t.startAndVerify(ctx, manager, c.Binary, t.Previous, true); err != nil {
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

// These sibling directories belong to this binary. Only their owner, holding
// the update lock, may remove them. Neither one is an uncommitted journal.
func cleanUpdateWork(pending string) error {
	if err := os.RemoveAll(pending + ".stage"); err != nil {
		return err
	}
	retired := pending + ".finished"
	if _, err := os.Lstat(retired); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(pending)); err != nil {
		return err
	}
	return os.RemoveAll(retired)
}

func finishUpdate(pending string) error {
	// Persist the commit (removal of the journal name) before deleting backups.
	retired := pending + ".finished"
	if err := os.Rename(pending, retired); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(pending)); err != nil {
		return err
	}
	return os.RemoveAll(retired)
}
