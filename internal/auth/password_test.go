package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestPasswordHasher(t *testing.T) {
	h := NewPasswordHasher(bcrypt.MinCost)

	hash1, err := h.Hash("my-password")
	if err != nil {
		t.Fatal(err)
	}
	hash2, _ := h.Hash("my-password")

	// Random salt: the same password never produces the same hash, so an
	// attacker cannot spot users who share a password.
	if hash1 == hash2 {
		t.Error("two hashes of the same password are identical: salt missing")
	}

	if ok, err := h.Matches(hash1, "my-password"); err != nil || !ok {
		t.Errorf("Matches(correct) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := h.Matches(hash1, "My-password"); err != nil || ok {
		t.Errorf("Matches(wrong) = %v, %v; want false, nil", ok, err)
	}
	if _, err := h.Matches("not-a-bcrypt-hash", "x"); err == nil {
		t.Error("Matches(corrupt hash) error = nil, want error")
	}
}
