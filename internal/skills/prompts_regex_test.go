package skills

import (
	"fmt"
	"sync"
	"testing"
)

func TestMatchRegex_ConcurrentAccess(t *testing.T) {
	const goroutines = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Each goroutine uses a unique pattern and a name that should match it.
	// Pattern: "^prefix_N$", name: "prefix_N" -> should match.
	// Also verify a non-matching name returns false.
	errs := make(chan string, goroutines*2)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			pattern := fmt.Sprintf("^prefix_%d$", idx)
			matchName := fmt.Sprintf("prefix_%d", idx)
			noMatchName := fmt.Sprintf("other_%d", idx)

			if !matchRegex(pattern, matchName) {
				errs <- fmt.Sprintf("goroutine %d: expected matchRegex(%q, %q) = true", idx, pattern, matchName)
			}
			if matchRegex(pattern, noMatchName) {
				errs <- fmt.Sprintf("goroutine %d: expected matchRegex(%q, %q) = false", idx, pattern, noMatchName)
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for msg := range errs {
		t.Error(msg)
	}
}
