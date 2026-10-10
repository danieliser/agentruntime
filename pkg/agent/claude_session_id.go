package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

// claudeProviderSessionID preserves canonical UUIDs and derives other logical
// IDs with RFC UUIDv5. The fixed URL namespace is 6ba7b811-9dad-11d1-80b4-00c04fd430c8;
// its name is the UTF-8 string "agentruntime:claude-session:" plus the logical ID.
// Only the provider command uses this identity; durable/session/container keys
// retain their original logical identity. Observed resume IDs never pass here.
func claudeProviderSessionID(logicalID string) string {
	if len(logicalID) == 36 && logicalID[8] == '-' && logicalID[13] == '-' && logicalID[18] == '-' && logicalID[23] == '-' {
		raw, err := hex.DecodeString(strings.ReplaceAll(logicalID, "-", ""))
		if err == nil && len(raw) == 16 && raw[6]>>4 >= 1 && raw[6]>>4 <= 8 && raw[8]&0xc0 == 0x80 {
			return logicalID
		}
	}
	namespace := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	hash := sha1.New() // RFC UUIDv5 identity derivation, not a security digest.
	_, _ = hash.Write(namespace)
	_, _ = hash.Write([]byte("agentruntime:claude-session:" + logicalID))
	id := hash.Sum(nil)[:16]
	id[6] = (id[6] & 0x0f) | 0x50
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}
