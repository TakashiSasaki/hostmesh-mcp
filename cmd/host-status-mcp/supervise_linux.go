package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

type supervisedChild struct {
	process *os.Process
	done    chan struct{}
	err     error // published by closing done
	started time.Time
}

// The supervisor event loop owns every child transition, including updates.
type supervisedPeer struct {
	settings updateSettings
	child    *supervisedChild
	restored bool // old files restored in this process, possibly awaiting an observer
}

func (p *supervisedPeer) start(ctx context.Context, recovering bool) error {
	p.restored = recovering
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.child != nil {
		select {
		case <-p.child.done:
			p.child = nil
		default:
			return nil
		}
	}
	c := p.settings
	cmd := exec.Command(c.Binary, "serve", "-config", c.PeerConfig)
	cmd.Dir = filepath.Dir(c.PeerConfig)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return err
	}
	child := &supervisedChild{process: cmd.Process, done: make(chan struct{}), started: time.Now()}
	p.child = child
	go func() {
		child.err = cmd.Wait()
		close(child.done)
	}()
	return nil
}

func (p *supervisedPeer) processDir(_ context.Context, binary string) (string, error) {
	if p.child == nil {
		return "", os.ErrProcessDone
	}
	select {
	case <-p.child.done:
		return "", os.ErrProcessDone
	default:
		return checkedProcessDir(strconv.Itoa(p.child.process.Pid), binary)
	}
}

func (p *supervisedPeer) stop(ctx context.Context) error {
	child := p.child
	if child == nil {
		return nil
	}
	if err := child.process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-child.done:
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
	select {
	case <-child.done:
	default:
		if err := child.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-child.done
	}
	p.child = nil
	return nil
}

func runSupervise(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("host-status-mcp supervise", flag.ContinueOnError)
	path := fs.String("config", "", "local updater configuration with peer_config")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *path == "" || fs.NArg() != 0 {
		return errors.New("usage: host-status-mcp supervise -config <updater.json>")
	}
	c, err := readUpdateSettings(*path)
	if err != nil {
		return err
	}
	if c.PeerConfig == "" {
		return errors.New("supervise requires peer_config in the updater configuration")
	}
	lock, err := lockUpdateFile(c.Binary+".update-lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer lock.Close()
	binary, err := os.Lstat(c.Binary)
	if err != nil {
		return err
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	if !binary.Mode().IsRegular() || os.SameFile(binary, self) {
		return errors.New("binary must be a separate regular file from the supervisor executable")
	}
	check := make(chan os.Signal, 1)
	signal.Notify(check, syscall.SIGUSR1)
	defer signal.Stop(check)
	// Linux sends Pdeathsig when the thread that forked the child exits.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := supervisedPeer{settings: c}
	err = p.monitor(ctx, check)
	return errors.Join(err, p.stop(context.Background()))
}

func (p *supervisedPeer) recoverPending(ctx context.Context) error {
	c := p.settings
	pending := c.Binary + ".update-pending"
	if err := cleanUpdateWork(pending); err != nil {
		return err
	}
	if _, err := os.Lstat(pending); errors.Is(err, os.ErrNotExist) {
		p.restored = false
		return nil
	} else if err != nil {
		return err
	}
	t, err := recoveryTarget(c, pending)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		if p.restored {
			// A peer-only observer can be offline after a reboot. Keep the
			// restored peer running and retain the journal until verified.
			err = t.startAndVerify(ctx, p, c.Binary, t.Previous, true)
			if err == nil {
				err = finishUpdate(pending)
			}
		} else {
			err = restoreUpdate(ctx, c, pending, p)
		}
		if err == nil {
			p.restored = false
			log.Printf("Recovered %s; verified node %s", t.Previous, t.NodeID)
			return nil
		}
		// A commit may already have renamed the journal before a sync or
		// cleanup error. Retry that cleanup without restoring the peer again.
		if _, statErr := os.Lstat(pending); errors.Is(statErr, os.ErrNotExist) {
			return cleanUpdateWork(pending)
		}
		log.Printf("Recovery pending: %v", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return ctx.Err()
}

func (p *supervisedPeer) monitor(ctx context.Context, check <-chan os.Signal) error {
	interval, err := p.settings.interval()
	if err != nil {
		return err
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	backoff := time.Second
	for ctx.Err() == nil {
		if err := p.recoverPending(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := p.start(ctx, false); err != nil {
			log.Printf("Start serve: %v", err)
		} else {
			child := p.child
			select {
			case <-ctx.Done():
				return nil
			case <-child.done:
				log.Printf("serve exited (pid %d): %v", child.process.Pid, child.err)
				p.child = nil
				if time.Since(child.started) >= time.Minute {
					backoff = time.Second
				}
			case <-timer.C:
				p.check(ctx, child)
				timer.Reset(interval)
				continue
			case <-check:
				p.check(ctx, child)
				timer.Reset(interval)
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
			backoff = min(30*time.Second, 2*backoff)
		}
	}
	return nil
}

func (p *supervisedPeer) check(ctx context.Context, child *supervisedChild) {
	// A slow feed must not delay restarting a crashed peer. After publishing
	// the journal, switching versions intentionally stops that peer.
	prep, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-child.done:
			cancel()
		case <-prep.Done():
		}
	}()
	if err := updatePeer(ctx, prep, p.settings, p, false); err != nil && ctx.Err() == nil {
		log.Printf("Update: %v", err)
	}
}
