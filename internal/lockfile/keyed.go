package lockfile

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// KeyedShards bounds the number of persistent lock inodes while ensuring the
// same logical key is always serialized across processes.
const KeyedShards = 64

// AcquireKeyed takes a lock for key from a fixed-size shard set under dir.
func AcquireKeyed(dir, key string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory %s: %w", dir, err)
	}
	sum := sha256.Sum256([]byte(key))
	shard := int(sum[0]) % KeyedShards
	return Acquire(filepath.Join(dir, fmt.Sprintf("%02d.lock", shard)))
}
