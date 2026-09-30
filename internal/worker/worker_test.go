package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestRun_RepeatsJobsAndStopsOnCancel(t *testing.T) {
	var ok, failing, panicking atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Millisecond,
			Job{Name: "ok", Run: func(context.Context) error { ok.Add(1); return nil }},
			// A failing job and a panicking job must not stop the others.
			Job{Name: "fails", Run: func(context.Context) error { failing.Add(1); return errors.New("boom") }},
			Job{Name: "panics", Run: func(context.Context) error { panicking.Add(1); panic("bug") }},
		)
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// Immediately once, then every 10ms for ~80ms: several runs each.
	for name, n := range map[string]int64{"ok": ok.Load(), "fails": failing.Load(), "panics": panicking.Load()} {
		if n < 3 {
			t.Errorf("job %q ran %d times, want several", name, n)
		}
	}
}
