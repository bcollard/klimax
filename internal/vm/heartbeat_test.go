package vm

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe sink for the captured log output. The
// heartbeat writes from its own goroutine while the test reads, and
// bytes.Buffer alone is not safe for that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog swaps the default logger for one writing to a buffer, and restores
// it when the test ends.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func withInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := heartbeatInterval
	heartbeatInterval = d
	t.Cleanup(func() { heartbeatInterval = prev })
}

func TestHeartbeatTicksAndCarriesDetailOnce(t *testing.T) {
	withInterval(t, 10*time.Millisecond)
	buf := captureSlog(t)

	stop := startHeartbeat(t.Context())
	time.Sleep(120 * time.Millisecond)
	stop() // blocks until the goroutine has exited

	out := buf.String()
	ticks := strings.Count(out, "Still starting the VM")
	if ticks < 2 {
		t.Fatalf("expected repeated ticks, got %d:\n%s", ticks, out)
	}
	// The flag hint is guidance, not something to repeat on every line.
	if n := strings.Count(out, "--show-vm-logs"); n != 1 {
		t.Errorf("detail hint should appear exactly once, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "elapsed=") {
		t.Errorf("every tick should report elapsed time:\n%s", out)
	}
}

func TestHeartbeatStopsOnStop(t *testing.T) {
	withInterval(t, 10*time.Millisecond)
	buf := captureSlog(t)

	stop := startHeartbeat(t.Context())
	time.Sleep(35 * time.Millisecond)
	stop()
	before := strings.Count(buf.String(), "Still starting")

	time.Sleep(60 * time.Millisecond)
	if after := strings.Count(buf.String(), "Still starting"); after != before {
		t.Errorf("heartbeat kept logging after stop: %d -> %d", before, after)
	}
}

// A start that finishes quickly — the common case on an already-provisioned VM —
// must not log anything at all.
func TestHeartbeatSilentWhenStartIsFast(t *testing.T) {
	withInterval(t, time.Hour)
	buf := captureSlog(t)

	stop := startHeartbeat(t.Context())
	stop()

	if out := buf.String(); strings.Contains(out, "Still starting") {
		t.Errorf("expected no output for a fast start, got:\n%s", out)
	}
}

// Cancelling the parent context must stop the goroutine even if stop() is never
// called.
func TestHeartbeatStopsWithParentContext(t *testing.T) {
	withInterval(t, 10*time.Millisecond)
	buf := captureSlog(t)

	ctx, cancel := context.WithCancel(t.Context())
	stop := startHeartbeat(ctx)
	t.Cleanup(stop)
	time.Sleep(35 * time.Millisecond)
	cancel()
	stop() // drains the goroutine that the parent cancel just released
	before := strings.Count(buf.String(), "Still starting")

	time.Sleep(60 * time.Millisecond)
	if after := strings.Count(buf.String(), "Still starting"); after != before {
		t.Errorf("heartbeat outlived its parent context: %d -> %d", before, after)
	}
}
