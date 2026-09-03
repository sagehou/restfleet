package server

import (
	"strings"
	"testing"
)

func TestValidPasswordLengthUsesUnicodeCodePoints(t *testing.T) {
	if validPasswordLength("short", 12) {
		t.Fatal("password below the minimum was accepted")
	}

	valid := strings.Repeat("界", maxPasswordRunes)
	if !validPasswordLength(valid, 1) {
		t.Fatal("valid Unicode password at the contract limit was rejected")
	}

	if validPasswordLength(valid+"界", 1) || validPasswordLength(string([]byte{0xff}), 1) {
		t.Fatal("password beyond the contract or UTF-8 limit was accepted")
	}
}
