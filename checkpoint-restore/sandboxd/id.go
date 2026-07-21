package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// sandboxId must be a DNS-label-ish token: safe to use as a path segment and a
// runsc container id. This closes the path-traversal hole (e.g. "../../etc").
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validateID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid sandboxId %q: must match %s", id, idRe.String())
	}
	return nil
}

// genID returns a fresh, collision-resistant, path-safe sandbox id.
func genID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "sb-" + hex.EncodeToString(b)
}

// ensureID validates a caller-supplied id or generates one. Returns ("",err) if
// the caller supplied an invalid id (we do NOT silently replace it — that would
// hide client bugs and break teleport identity).
func ensureID(supplied string) (string, error) {
	if supplied == "" {
		return genID(), nil
	}
	if err := validateID(supplied); err != nil {
		return "", err
	}
	return supplied, nil
}
