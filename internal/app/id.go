// Package app is crucible's control-plane layer: durable, named apps whose
// desired state the daemon reconciles into a running instance (a sandbox),
// so a workload survives a daemon restart or host reboot by being
// re-created from spec. It sits above internal/sandbox — an app owns an
// instance; it does not replace the sandbox primitive.
//
// This file: app identity. IDs are self-identifying (app_...) like
// sandbox/snapshot IDs; names are the stable user-facing handle and are
// validated as DNS labels because they become routing hostnames.
package app

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"regexp"
	"strings"
)

const appIDPrefix = "app_"

// idRandomBytes matches internal/sandbox: 8 bytes = 64 bits, ample for a
// single host, 13 base32 chars of suffix.
const idRandomBytes = 8

// NewID returns a fresh app identifier of the form `app_xxx...`.
func NewID() (string, error) {
	var buf [idRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("app: generate id: %w", err)
	}
	enc := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:])
	return appIDPrefix + strings.ToLower(enc), nil
}

// IsValidID reports whether s has the shape of an app ID.
func IsValidID(s string) bool {
	if !strings.HasPrefix(s, appIDPrefix) {
		return false
	}
	suffix := strings.ToUpper(strings.TrimPrefix(s, appIDPrefix))
	if suffix == "" {
		return false
	}
	_, err := base32.HexEncoding.WithPadding(base32.NoPadding).DecodeString(suffix)
	return err == nil
}

// validName is a DNS label: lowercase alphanumeric and hyphens, 1–40
// chars, no leading/trailing hyphen. Apps are addressed and (in a later
// release) routed by name, so the name must be a valid hostname label from
// day one.
var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// IsValidName reports whether s is a usable app name.
func IsValidName(s string) bool { return validName.MatchString(s) }
