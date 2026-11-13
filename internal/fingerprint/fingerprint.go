// Package fingerprint gives every failure a repeatable name.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
)

// domain separates these hashes from any other use of SHA-256 and names the
// version of the recipe. Changing the recipe means changing this string.
const domain = "bellbuoy/fingerprint/v1"

// Of returns the fingerprint of a failure: the SHA-256, in lower-case hex, of
// the domain string, the worker name, the error code and the sanitized
// failure pattern. Each part is written as its length in bytes (eight bytes,
// big-endian) followed by the bytes, so no two different inputs can run
// together into the same stream.
//
// Two failures that differ only in numbers, addresses, host names or other
// identifiers have the same pattern, and so the same fingerprint.
func Of(worker string, code codes.Code, pattern string) string {
	h := sha256.New()
	var n [8]byte
	for _, part := range [...]string{domain, worker, string(code), sanitize.Pattern(pattern)} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Short returns the first twelve characters, for titles.
func Short(fp string) string {
	if len(fp) < 12 {
		return fp
	}
	return fp[:12]
}
