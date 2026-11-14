package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
)

// minKeyLength rejects gateway keys short enough to guess.
const minKeyLength = 16

// parseKeys reads GATEWAY_KEYS, a list like "fineprint:KEY1,notebook:KEY2".
// It returns a map from the SHA-256 hash of each key to the caller's name.
// Looking a key up by its hash means no step compares the secret one
// character at a time, so the time a lookup takes tells a guesser nothing.
func parseKeys(spec string) (map[[32]byte]string, error) {
	keys := map[[32]byte]string{}
	names := map[string]bool{}
	for _, entry := range strings.Split(spec, ",") {
		name, key, found := strings.Cut(entry, ":")
		name, key = strings.TrimSpace(name), strings.TrimSpace(key)
		if !found || name == "" {
			return nil, fmt.Errorf("each entry must look like name:key")
		}
		if len(key) < minKeyLength {
			return nil, fmt.Errorf("the key for %q is shorter than %d characters", name, minKeyLength)
		}
		hash := sha256.Sum256([]byte(key))
		if names[name] || keys[hash] != "" {
			return nil, fmt.Errorf("%q is listed twice, or shares its key with another caller", name)
		}
		names[name] = true
		keys[hash] = name
	}
	return keys, nil
}

// caller returns the name that owns the gateway key on this request. The key
// is read from the same two headers the Messages API takes a key in, so a
// client only has to swap its API key for a gateway key.
func (g *gateway) caller(r *http.Request) (string, bool) {
	name, ok := g.keys[sha256.Sum256([]byte(presentedKey(r)))]
	return name, ok
}

// presentedKey is the key a request carries, or "" when it carries none.
func presentedKey(r *http.Request) string {
	if key := r.Header.Get("X-Api-Key"); key != "" {
		return key
	}
	// The scheme word may be written in any case (RFC 9110, 11.1).
	scheme, rest, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}
