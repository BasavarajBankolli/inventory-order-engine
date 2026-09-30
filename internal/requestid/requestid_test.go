package requestid

import (
	"context"
	"strings"
	"testing"
)

func TestNew_IsUniqueAndValid(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := New()
		if len(id) != 32 {
			t.Fatalf("len(New()) = %d, want 32", len(id))
		}
		if !IsValid(id) {
			t.Fatalf("New() produced invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("New() produced duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := NewContext(context.Background(), "abc-123")
	if got := FromContext(ctx); got != "abc-123" {
		t.Errorf("FromContext() = %q, want abc-123", got)
	}
	if got := FromContext(context.Background()); got != "" {
		t.Errorf("FromContext(empty) = %q, want empty string", got)
	}
}

func TestIsValid(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"abc-123_X.y", true},
		{"", false},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"has space", false},
		{"new\nline", false}, // would allow fake log lines (log injection)
		{"<script>", false},
	}
	for _, tt := range tests {
		if got := IsValid(tt.id); got != tt.want {
			t.Errorf("IsValid(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}
