package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Verified markers.
//
// A cosign check proves that one digest carries a valid signature for one
// public key. Both are immutable, so the fact holds for as long as the cached
// content does. After a successful check the materializer records it next to
// the cache entry, as .verified/<digest>/<sha256 of the key bytes>. A warm-pool
// checkout (which runs no cosign) projects an entry for a sandbox with a key
// only when the marker for exactly that key exists; otherwise the entry is
// fetched and verified again with that key.
//
// The marker's name is computed here from the key file the check used, never
// taken from input, and only the privileged materializer writes the cache:
// launchers mount it read-only.

const verifiedDir = ".verified"

// KeyFingerprint identifies a cosign public key: sha256 of its bytes, hex.
func KeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

// VerifiedMarkerPath is where the check of digest with the key whose
// fingerprint is keyFingerprint is recorded.
func VerifiedMarkerPath(cacheDir, digest, keyFingerprint string) string {
	return filepath.Join(cacheDir, verifiedDir, strings.ReplaceAll(digest, ":", "-"), keyFingerprint)
}

// RecordVerified records that digest verified with the key in keyFile.
// Idempotent; the marker appears atomically.
func RecordVerified(cacheDir, digest, keyFile string) error {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return fmt.Errorf("record verification: digest %q is not sha256", digest)
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("record verification: %w", err)
	}
	marker := VerifiedMarkerPath(cacheDir, digest, KeyFingerprint(key))
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	dir := filepath.Dir(marker)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("record verification: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".marker-*")
	if err != nil {
		return fmt.Errorf("record verification: %w", err)
	}
	name := tmp.Name()
	tmp.Close()
	if err := os.Chmod(name, 0o444); err != nil {
		os.Remove(name)
		return fmt.Errorf("record verification: %w", err)
	}
	if err := os.Rename(name, marker); err != nil {
		os.Remove(name)
		return fmt.Errorf("record verification: %w", err)
	}
	return nil
}
