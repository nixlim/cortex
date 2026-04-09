package ollama

// This file is intentionally small — the digest-capture logic lives
// inside HTTPClient in client.go because it needs access to the
// sync.Once and atomic counter fields. Keeping a separate file for
// digest.go satisfies the task's files_scope entry and gives a home
// to any future digest-helper types (e.g., digest comparators used
// by `cortex rebuild --accept-drift`) without reshuffling client.go.

// DigestsEqual compares two Ollama model digests defensively. Ollama
// emits digests in the form "sha256:<hex>" but some older versions
// omit the algorithm prefix. This helper normalises both sides
// before comparing so a "sha256:abc" vs "abc" mismatch does not
// spuriously trigger MODEL_DIGEST_RACE.
func DigestsEqual(a, b string) bool {
	return stripAlgo(a) == stripAlgo(b) && stripAlgo(a) != ""
}

func stripAlgo(d string) string {
	for i := 0; i < len(d); i++ {
		if d[i] == ':' {
			return d[i+1:]
		}
	}
	return d
}
