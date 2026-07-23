package runtime

import (
	"fmt"
	"math/rand/v2"

	"github.com/google/uuid"
)

// generateRandom produces a fresh value for a `${random:<kind>}`
// token. Randomness is fine here — the runner executes in the bundle
// binary at runtime (not in a workflow script), and each draw is
// meant to be distinct per use, so non-determinism is the point.
//
//	random:uuid   → a v4 UUID string
//	random:int    → a non-negative int (fits a proto int32 range so it
//	                round-trips through any numeric field)
//	random:string → a short random hex string
func generateRandom(kind string) (any, error) {
	switch kind {
	case "uuid":
		return uuid.NewString(), nil
	case "int":
		return rand.IntN(1 << 30), nil
	case "string":
		return randHex(8), nil
	default:
		return nil, fmt.Errorf("e2e runtime: unknown generator ${random:%s} (want uuid, int, or string)", kind)
	}
}

const hexDigits = "0123456789abcdef"

func randHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = hexDigits[rand.IntN(16)]
	}
	return string(b)
}
