// Package pagination applies --limit and --offset to ordered result lists.
//
// The helper is deliberately generic and side-effect-free. It assumes the
// caller has already ordered the slice stably; it guarantees the same
// (slice, limit, offset) inputs produce the same slice of pointers back,
// which is what "deterministic pagination" means in the BDD scenario.
package pagination

import "errors"

// ErrBadPagination is returned for invalid limit/offset combinations.
// Specifically: negative offset, or non-positive limit.
var ErrBadPagination = errors.New("pagination: invalid limit or offset")

// Page returns the slice window [offset, offset+limit) from results,
// clamped to the end of the slice. A page that runs past the end simply
// returns what is available; an empty result for an out-of-range offset
// is not an error.
func Page[T any](results []T, limit, offset int) ([]T, error) {
	if offset < 0 || limit <= 0 {
		return nil, ErrBadPagination
	}
	if offset >= len(results) {
		return []T{}, nil
	}
	end := offset + limit
	if end > len(results) {
		end = len(results)
	}
	// Return a copy so later caller mutations cannot affect the source.
	out := make([]T, end-offset)
	copy(out, results[offset:end])
	return out, nil
}
