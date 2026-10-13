package runtime

import (
	"sync"
	"testing"
)

// `${seq}` is unique across everything one runner process does at once:
// several runs and scopes expanding concurrently never draw the same
// value. (Two runner processes can; see the deliberate limits of T21.)
func TestSeq_UniqueAcrossConcurrentRunsAndScopes(t *testing.T) {
	resetProcessSeq()
	const runs, scopes, draws = 2, 8, 200
	var (
		mu   sync.Mutex
		seen = map[any]int{}
		wg   sync.WaitGroup
	)
	for range runs {
		run := NewRun()
		for range scopes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s := run.NewScope()
				for range draws {
					v, err := s.expandString("${seq}")
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					seen[v]++
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(seen) != runs*scopes*draws {
		dups := 0
		for _, n := range seen {
			if n > 1 {
				dups++
			}
		}
		t.Fatalf("%d distinct values from %d draws (%d drawn more than once)", len(seen), runs*scopes*draws, dups)
	}
}
