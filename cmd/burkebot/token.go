package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Token authorizes a single API caller. The cleartext token never lives
// on the burkebot host; only the sha256 of it does. Tokens are scoped to
// a fixed list of task names — the caller cannot widen its capability.
type Token struct {
	// Name is a human label (e.g. "public-meetings") used in audit
	// bundles and logs, never in the wire protocol.
	Name string `json:"name"`

	// SHA256Hex is the lowercase hex sha256 of the cleartext token.
	// Generated once at provisioning time:
	//   echo -n "$token" | sha256sum
	SHA256Hex string `json:"sha256_hex"`

	// AllowedTasks is the whitelist of Task.Name values this token may
	// invoke. Empty means "no tasks allowed" (not "all tasks") — there
	// is deliberately no wildcard.
	AllowedTasks []string `json:"allowed_tasks"`
}

// loadTokens reads tokens.json from disk. Returns nil if path is empty
// or the file does not exist (the API requires both tasks and tokens to
// be loaded; if either is missing the API stays disabled).
func loadTokens(path string) ([]Token, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tokens []Token
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for i, t := range tokens {
		if t.Name == "" {
			return nil, fmt.Errorf("token %d has no name", i)
		}
		if len(t.SHA256Hex) != 64 {
			return nil, fmt.Errorf("token %q: sha256_hex must be 64 hex chars (got %d)", t.Name, len(t.SHA256Hex))
		}
		if _, err := hex.DecodeString(t.SHA256Hex); err != nil {
			return nil, fmt.Errorf("token %q: sha256_hex is not valid hex: %w", t.Name, err)
		}
	}
	return tokens, nil
}

// authorizeToken finds the Token whose hash matches bearer (constant
// time) and confirms it is allowed to invoke taskName. Returns the
// matching token on success; both nil values mean "deny" but the caller
// should respond identically in either case to avoid leaking which
// arm failed.
func authorizeToken(tokens []Token, bearer, taskName string) *Token {
	if bearer == "" || taskName == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(bearer))
	got := []byte(hex.EncodeToString(sum[:]))

	var match *Token
	for i := range tokens {
		want := []byte(strings.ToLower(tokens[i].SHA256Hex))
		// constant-time compare every entry so timing doesn't reveal
		// which (if any) token matched.
		if subtle.ConstantTimeCompare(got, want) == 1 {
			match = &tokens[i]
		}
	}
	if match == nil {
		return nil
	}
	if !slices.Contains(match.AllowedTasks, taskName) {
		return nil
	}
	return match
}

// extractBearer pulls the token out of an `Authorization: Bearer …`
// header. Returns "" if the header is missing or malformed.
func extractBearer(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
