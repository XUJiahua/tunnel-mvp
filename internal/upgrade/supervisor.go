package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

type Options struct {
	MinimumSequence                                      uint64
	Directory, PublicKey, ManifestURL, Bootstrap, Config string
	Poll, ReadyTimeout                                   time.Duration
	WatchConfig                                          bool
	Output                                               io.Writer
}
type child struct {
	cmd   *exec.Cmd
	done  chan struct{}
	ready chan struct{}
	err   error
}

// logWriter forwards JSON logs while observing the existing successful tunnel
// connection event. Readiness is local telemetry from a signed, trusted binary.
type logWriter struct {
	mu              sync.Mutex
	pending         []byte
	out             io.Writer
	ready           chan struct{}
	once            sync.Once
	expectedVersion string
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out != nil {
		_, _ = w.out.Write(b)
	}
	for _, c := range b {
		if c == '\n' {
			var event struct {
				Message string `json:"msg"`
				Version string `json:"version"`
			}
			if json.Unmarshal(w.pending, &event) == nil && event.Message == "site tunnel connected" && (w.expectedVersion == "" || event.Version == w.expectedVersion) {
				w.once.Do(func() { close(w.ready) })
			}
			w.pending = w.pending[:0]
		} else if len(w.pending) < 65536 {
			w.pending = append(w.pending, c)
		}
	}
	return len(b), nil
}
func startChild(path string, o Options, expectedVersion string) (*child, error) {
	args := []string{"agent", "--config", o.Config, "--log-format", "json", "--log-level", "info"}
	if o.WatchConfig {
		args = append(args, "--watch-config")
	}
	c := &child{cmd: exec.Command(path, args...), done: make(chan struct{}), ready: make(chan struct{})}
	c.cmd.Env = os.Environ()
	writer := &logWriter{out: o.Output, ready: c.ready, expectedVersion: expectedVersion}
	c.cmd.Stdout = writer
	c.cmd.Stderr = writer
	if e := c.cmd.Start(); e != nil {
		return nil, e
	}
	go func() { c.err = c.cmd.Wait(); close(c.done) }()
	return c, nil
}
func (c *child) stop() {
	if c == nil {
		return
	}
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Signal(os.Interrupt)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}
func (c *child) await(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("Agent exited before readiness: %v", c.err)
	case <-timer.C:
		return fmt.Errorf("Agent readiness timed out")
	case <-c.ready:
		select {
		case <-c.done:
			return fmt.Errorf("Agent exited at readiness")
		default:
			return nil
		}
	}
}
func Supervise(ctx context.Context, o Options) error {
	if o.Directory == "" || o.Config == "" || o.PublicKey == "" || o.ManifestURL == "" {
		return fmt.Errorf("update directory, config, pinned public key and manifest URL are required")
	}
	if e := validURL(o.ManifestURL); e != nil {
		return e
	}
	if o.Poll <= 0 {
		o.Poll = time.Minute
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 30 * time.Second
	}
	m, e := Open(o.Directory, o.PublicKey, o.Bootstrap)
	if e != nil {
		return e
	}
	defer m.Close()
	if o.MinimumSequence > m.record.HighWater {
		m.record.HighWater = o.MinimumSequence
		if e = m.save(); e != nil {
			return e
		}
	}
	var running *child
	defer func() { running.stop() }()
	boot := func() error {
		var err error
		expected := m.Status().CurrentVersion
		if m.Status().Pending {
			expected = m.Status().CandidateVersion
		}
		if expected == "bootstrap" {
			expected = ""
		}
		running, err = startChild(m.Current(), o, expected)
		if err != nil {
			return err
		}
		return running.await(ctx, o.ReadyTimeout)
	}
	if e = boot(); e != nil {
		running.stop()
		if ctx.Err() != nil {
			return nil
		}
		if re := m.Rollback(); re != nil {
			return fmt.Errorf("Agent startup failed (%v); no verified rollback: %w", e, re)
		}
		if e = boot(); e != nil {
			return e
		}
		slog.Warn("Agent startup failed; restored previous executable")
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-running.done:
			failures++
			if failures >= 3 {
				if e = m.Rollback(); e != nil {
					return fmt.Errorf("Agent repeatedly exited; rollback unavailable: %w", e)
				}
				failures = 0
				slog.Warn("Agent repeatedly exited; rolling back")
			}
			retry := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				retry.Stop()
				return nil
			case <-retry.C:
			}
			if e = boot(); e != nil {
				running.stop()
				if ctx.Err() != nil {
					return nil
				}
				if re := m.Rollback(); re != nil {
					return fmt.Errorf("Agent restart failed: %w", e)
				}
				if e = boot(); e != nil {
					return e
				}
			}
		case <-timer.C:
			release, err := m.Check(ctx, o.ManifestURL)
			if err != nil {
				slog.Warn("signed update check failed", "error", err)
			} else if release != nil {
				if err = m.Stage(ctx, release); err != nil {
					slog.Warn("signed update staging failed", "error", err)
				} else {
					running.stop()
					if err = m.Install(release); err != nil {
						if re := m.Recover(); re != nil {
							return re
						}
						if e = boot(); e != nil {
							return e
						}
						slog.Warn("Agent update install failed; retained previous executable", "error", err)
					} else {
						if err = boot(); err != nil {
							running.stop()
							if e = m.Recover(); e != nil {
								return e
							}
							if ctx.Err() != nil {
								return nil
							}
							if e = boot(); e != nil {
								return fmt.Errorf("rollback failed to become ready: %w", e)
							}
							slog.Warn("Agent update failed readiness; rolled back", "version", release.Version(), "error", err)
						} else {
							if e = m.Commit(); e != nil {
								return e
							}
							failures = 0
							slog.Info("Agent update healthy", "version", release.Version(), "sequence", release.Sequence())
						}
					}
				}
			}
			timer.Reset(o.Poll)
		}
	}
}
