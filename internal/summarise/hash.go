package summarise

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// MembershipHash returns the canonical SHA-256 hex digest of a
// community's entry id set. The caller must pass the same logical
// community — the function does not care whether ids are already
// sorted; it sorts internally so callers who produce order-varying
// inputs still get stable hashes.
//
// The hash goes into CommunityBrief.slots.membership_hash and is
// compared to the prior brief on subsequent runs; a match means the
// cluster membership has not changed and the LLM call can be
// skipped. (Order-independent so a caller that yields ids in
// arrival-tx order vs sorted order still lands on the same hash.)
func MembershipHash(entryIDs []string) string {
	ids := append([]string(nil), entryIDs...)
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		// Length-prefix each id with a NUL separator so
		// ["ab","c"] and ["a","bc"] hash to different digests.
		h.Write([]byte{0})
		h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil))
}
