package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"inventory-order-engine/internal/identity"
	"inventory-order-engine/internal/requestid"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	return m
}

func TestLogger_AddsRequestAndUserFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo)

	ctx := requestid.NewContext(context.Background(), "req-7")
	ctx = identity.NewContext(ctx, identity.Principal{UserID: 42})
	logger.InfoContext(ctx, "hello", "order_id", 9)

	line := decode(t, &buf)
	if line["request_id"] != "req-7" || line["user_id"] != float64(42) || line["order_id"] != float64(9) {
		t.Errorf("log line = %v", line)
	}
}

// Loggers derived with With(...) (as cmd/worker does) must keep adding the
// context values - that is why WithAttrs/WithGroup are overridden.
func TestLogger_DerivedLoggersKeepContextValues(t *testing.T) {
	ctx := requestid.NewContext(context.Background(), "req-8")

	var buf bytes.Buffer
	New(&buf, slog.LevelInfo).With("service", "worker").InfoContext(ctx, "ran")
	line := decode(t, &buf)
	if line["service"] != "worker" || line["request_id"] != "req-8" {
		t.Errorf("With(): log line = %v", line)
	}

	// With a group, slog nests later attributes (including request_id)
	// inside the group - but they must still be there.
	buf.Reset()
	New(&buf, slog.LevelInfo).WithGroup("job").InfoContext(ctx, "ran", "n", 1)
	group, _ := decode(t, &buf)["job"].(map[string]any)
	if group["request_id"] != "req-8" || group["n"] != float64(1) {
		t.Errorf("WithGroup(): log line = %s", buf.String())
	}
}

func TestLogger_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelWarn).Info("too quiet")
	if buf.Len() != 0 {
		t.Errorf("INFO logged at WARN level: %s", buf.String())
	}
}
