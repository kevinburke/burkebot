package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestAuthorizeToken(t *testing.T) {
	tokens := []Token{
		{Name: "alice", SHA256Hex: sha256Hex("alice-secret"), AllowedTasks: []string{"task-a"}},
		{Name: "bob", SHA256Hex: sha256Hex("bob-secret"), AllowedTasks: []string{"task-b", "task-c"}},
	}

	cases := []struct {
		name    string
		bearer  string
		task    string
		wantTok string // "" means deny
	}{
		{"alice ok", "alice-secret", "task-a", "alice"},
		{"alice wrong task", "alice-secret", "task-b", ""},
		{"bob multi-task", "bob-secret", "task-c", "bob"},
		{"unknown bearer", "fake", "task-a", ""},
		{"empty bearer", "", "task-a", ""},
		{"empty task", "alice-secret", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := authorizeToken(tokens, c.bearer, c.task)
			if c.wantTok == "" && got != nil {
				t.Fatalf("expected deny, got %q", got.Name)
			}
			if c.wantTok != "" && (got == nil || got.Name != c.wantTok) {
				t.Fatalf("expected %q, got %v", c.wantTok, got)
			}
		})
	}
}

func TestExtractBearer(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":      "abc",
		"Bearer  spaced ": "spaced",
		"bearer abc":      "", // case-sensitive per RFC
		"Basic abc":       "",
		"":                "",
	}
	for in, want := range cases {
		if got := extractBearer(in); got != want {
			t.Errorf("extractBearer(%q) = %q, want %q", in, got, want)
		}
	}
}
