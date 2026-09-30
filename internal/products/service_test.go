package products

import (
	"errors"
	"strings"
	"testing"

	"inventory-order-engine/internal/validate"
)

// Unit tests for the pure normalisation/validation functions (no database).

func fieldErrors(t *testing.T, err error) validate.Errors {
	t.Helper()
	if err == nil {
		return nil
	}
	var verr validate.Errors
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want validate.Errors", err)
	}
	return verr
}

func validCreate() CreateInput {
	return CreateInput{SKU: "TSHIRT-RED-M", Name: "Red T-Shirt", Price: 49900, Currency: "INR"}
}

func TestNormalizeCreate(t *testing.T) {
	in := normalizeCreate(CreateInput{SKU: " tshirt-red-m ", Name: "  Shirt ", Currency: "inr"})

	if in.SKU != "TSHIRT-RED-M" || in.Name != "Shirt" || in.Currency != "INR" {
		t.Errorf("not normalised: %+v", in)
	}
	if in.Status != StatusActive {
		t.Errorf("default status = %q, want ACTIVE", in.Status)
	}
}

func TestValidateCreate(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*CreateInput)
		wantField string // "" = valid
	}{
		{"valid", func(*CreateInput) {}, ""},
		{"inactive allowed", func(in *CreateInput) { in.Status = StatusInactive }, ""},
		{"sku too short", func(in *CreateInput) { in.SKU = "A" }, "sku"},
		{"sku with space", func(in *CreateInput) { in.SKU = "RED SHIRT" }, "sku"},
		{"sku lowercase (not normalised)", func(in *CreateInput) { in.SKU = "red" }, "sku"},
		{"empty name", func(in *CreateInput) { in.Name = "" }, "name"},
		{"long name", func(in *CreateInput) { in.Name = strings.Repeat("x", 201) }, "name"},
		{"long description", func(in *CreateInput) { in.Description = strings.Repeat("x", 2001) }, "description"},
		{"zero price", func(in *CreateInput) { in.Price = 0 }, "price"},
		{"negative price", func(in *CreateInput) { in.Price = -100 }, "price"},
		{"price too large", func(in *CreateInput) { in.Price = maxPrice + 1 }, "price"},
		{"bad currency", func(in *CreateInput) { in.Currency = "RUPEES" }, "currency"},
		{"cannot create archived", func(in *CreateInput) { in.Status = StatusArchived }, "status"},
		{"unknown status", func(in *CreateInput) { in.Status = "DRAFT" }, "status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCreate()
			in.Status = StatusActive
			tt.modify(&in)

			errs := fieldErrors(t, validateCreate(in))
			if tt.wantField == "" {
				if errs != nil {
					t.Errorf("unexpected errors: %v", errs)
				}
				return
			}
			if _, ok := errs[tt.wantField]; !ok {
				t.Errorf("errors = %v, want a problem for %q", errs, tt.wantField)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestValidateUpdate(t *testing.T) {
	if errs := fieldErrors(t, validateUpdate(UpdateInput{})); errs["body"] == "" {
		t.Errorf("empty update: errors = %v, want a 'body' problem", errs)
	}

	// Clearing the description is a legitimate update.
	if err := validateUpdate(UpdateInput{Description: ptr("")}); err != nil {
		t.Errorf("clearing description: %v", err)
	}

	if errs := fieldErrors(t, validateUpdate(UpdateInput{Name: ptr("")})); errs["name"] == "" {
		t.Errorf("blank name: errors = %v", errs)
	}
	if errs := fieldErrors(t, validateUpdate(UpdateInput{Price: ptr[int64](0)})); errs["price"] == "" {
		t.Errorf("zero price: errors = %v", errs)
	}
	if errs := fieldErrors(t, validateUpdate(UpdateInput{Status: ptr(StatusArchived)})); errs["status"] == "" {
		t.Errorf("archive via PATCH must be rejected: errors = %v", errs)
	}
}

func TestListParams(t *testing.T) {
	p := normalizeList(ListParams{Search: "  shirt  "})
	if p.Search != "shirt" || p.Status != StatusActive || p.Sort != "newest" || p.Limit != defaultLimit {
		t.Errorf("defaults not applied: %+v", p)
	}
	if err := validateList(p); err != nil {
		t.Errorf("defaults should be valid: %v", err)
	}

	bad := []struct {
		field string
		p     ListParams
	}{
		{"limit", ListParams{Status: StatusActive, Sort: "newest", Limit: 101}},
		{"limit", ListParams{Status: StatusActive, Sort: "newest", Limit: -1}},
		{"offset", ListParams{Status: StatusActive, Sort: "newest", Limit: 10, Offset: -1}},
		{"sort", ListParams{Status: StatusActive, Sort: "price; DROP TABLE products", Limit: 10}},
		{"status", ListParams{Status: StatusArchived, Sort: "newest", Limit: 10}},
		{"q", ListParams{Status: StatusActive, Sort: "newest", Limit: 10, Search: strings.Repeat("q", 101)}},
	}
	for _, tt := range bad {
		if errs := fieldErrors(t, validateList(tt.p)); errs[tt.field] == "" {
			t.Errorf("%+v: errors = %v, want a problem for %q", tt.p, errs, tt.field)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	tests := map[string]string{
		"shirt":    "shirt",
		"50%":      `50\%`,
		"a_b":      `a\_b`,
		`back\sl`:  `back\\sl`,
		`%_\mixed`: `\%\_\\mixed`,
	}
	for in, want := range tests {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}
