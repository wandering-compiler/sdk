package acllock

import "sort"

// Allocate computes the new Lock from a previous snapshot + the
// current set of permission strings. Pure deterministic function
// — same inputs produce the same output regardless of map
// iteration ordering or wall-clock state.
//
// Invariants (mirror events lock allocator):
//
//  1. Existing permissions keep their assigned IDs.
//  2. New permissions (in `current`, not in `prev.Permissions`)
//     get the smallest positive integer not already used by an
//     existing ID and not in `prev.Reserved`.
//  3. Removed permissions (in `prev.Permissions`, not in
//     `current`) have their IDs moved into `Reserved`.
//  4. `Reserved` is monotonic — once an ID lands there, it
//     never leaves. Allocate enforces this by union-merging
//     prev's reserved set with the removed-ID set.
//
// The caller controls iteration order via `current`: new
// permissions receive IDs in `current` slice order. For
// determinism across runs, callers should pass permissions in a
// stable order (lexicographic is typical + matches the
// human-readable diff in PRs).
//
// Allocate doesn't set Checksum on the returned Lock — checksum
// computation + persistence live in the gateway's `gatewayacllock`
// package, not here. Callers that need a persisted, checksummed lock
// go through that package's render/write path.
func Allocate(prev *Lock, current []string) *Lock {
	if prev == nil {
		prev = &Lock{Version: CurrentVersion, Permissions: map[string]int{}}
	}

	next := &Lock{
		Version:     CurrentVersion,
		Permissions: make(map[string]int, len(current)),
	}

	used := map[int]bool{}
	for _, id := range prev.Permissions {
		used[id] = true
	}
	for _, r := range prev.Reserved {
		used[r] = true
	}

	currentSet := make(map[string]bool, len(current))
	for _, perm := range current {
		if perm == "" {
			continue
		}
		currentSet[perm] = true
		if id, ok := prev.Permissions[perm]; ok {
			next.Permissions[perm] = id
			continue
		}
		id := nextFreeID(used)
		next.Permissions[perm] = id
		used[id] = true
	}

	reservedSet := map[int]bool{}
	for _, r := range prev.Reserved {
		reservedSet[r] = true
	}
	for perm, id := range prev.Permissions {
		if currentSet[perm] {
			continue
		}
		reservedSet[id] = true
	}

	if len(reservedSet) > 0 {
		next.Reserved = make([]int, 0, len(reservedSet))
		for r := range reservedSet {
			next.Reserved = append(next.Reserved, r)
		}
		next.Reserved = sortAndDedupReserved(next.Reserved)
	}
	return next
}

// sortAndDedupReserved sorts ascending + removes duplicates.
// The allocator uses this on the Reserved slice so the
// stored output is stable across runs (deterministic input
// to the lock proto checksum).
func sortAndDedupReserved(in []int) []int {
	if len(in) == 0 {
		return nil
	}
	out := append([]int(nil), in...)
	sort.Ints(out)
	w := 0
	for r := 0; r < len(out); r++ {
		if w == 0 || out[r] != out[w-1] {
			out[w] = out[r]
			w++
		}
	}
	return out[:w]
}

// nextFreeID returns the smallest positive integer not in
// `used`. Linear scan from 1 upward — same trade-off as events
// lock's nextFreeTag (O(n) per call, O(n²) per Allocate; perm
// counts per domain are bounded so the constant factor stays
// well below noise).
func nextFreeID(used map[int]bool) int {
	for i := 1; ; i++ {
		if !used[i] {
			return i
		}
	}
}
