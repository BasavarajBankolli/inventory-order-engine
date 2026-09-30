package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	type target struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	tests := []struct {
		name    string
		body    string
		wantErr string // substring; "" means success
	}{
		{"valid", `{"name":"a","age":3}`, ""},
		{"empty body", ``, "must not be empty"},
		{"malformed", `{"name":`, "malformed JSON"},
		{"wrong type", `{"age":"three"}`, `field "age" has the wrong type`},
		{"unknown field", `{"nmae":"typo"}`, `unknown field "nmae"`},
		{"two objects", `{"name":"a"}{"name":"b"}`, "single JSON object"},
		{"too large", `{"name":"` + strings.Repeat("x", maxBodyBytes) + `"}`, "must not be larger"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", strings.NewReader(tt.body))
			var dst target
			err := DecodeJSON(httptest.NewRecorder(), req, &dst)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("DecodeJSON() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("DecodeJSON() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
