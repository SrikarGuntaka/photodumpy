package ingest

import (
	"context"
	"sync"
)

// passResult reports what a bounded pass did.
type passResult struct {
	Done        int
	Failed      int
	Interrupted bool
	FirstErr    error
}

// runPass drains a work queue with bounded concurrency.
//
// This exists because metadata extraction, content hashing and perceptual
// hashing are the same scheduling problem three times: claim a batch, run it
// under a semaphore, repeat until empty. Writing it three times is how the
// batch-boundary bug in Phase 4 came to exist in two places at once.
//
// Two properties the loop must preserve, both learned the hard way:
//
//  1. The semaphore slot is acquired BEFORE the goroutine is spawned, so the
//     number of live goroutines is capped rather than merely their throughput.
//     A 100,000-photo library must not spawn 100,000 goroutines.
//
//  2. Each batch is fully awaited before the next is claimed. Without that,
//     the loop re-queries while the batch is still in flight; those goroutines
//     have not written their completion marker yet, so the same rows come back
//     and are processed twice. That bug was invisible in the data because the
//     writes are idempotent -- only an exact-count assertion caught it.
//
// fetch returns the next batch, or an empty slice when there is no work left.
// process handles one item; its error is counted, not fatal, because one
// unreadable file should not abandon the other 9,999.
func runPass[T any](
	ctx context.Context,
	concurrency int,
	batchSize int,
	fetch func(context.Context, int) ([]T, error),
	process func(context.Context, T) error,
	onError func(item T, err error),
) passResult {
	var res passResult

	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)

	var mu sync.Mutex
	var wg sync.WaitGroup

	for {
		if ctx.Err() != nil {
			res.Interrupted = true
			break
		}

		batch, err := fetch(ctx, batchSize)
		if err != nil {
			if ctx.Err() != nil {
				res.Interrupted = true
				break
			}
			res.FirstErr = err
			break
		}
		if len(batch) == 0 {
			break
		}

		for i := range batch {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				res.Interrupted = true
			}
			if ctx.Err() != nil {
				break
			}

			wg.Add(1)
			go func(item T) {
				defer wg.Done()
				defer func() { <-sem }()

				err := process(ctx, item)

				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					// A cancelled context is a shutdown, not a failure.
					if ctx.Err() == nil {
						res.Failed++
						if res.FirstErr == nil {
							res.FirstErr = err
						}
						if onError != nil {
							onError(item, err)
						}
					}
					return
				}
				res.Done++
			}(batch[i])
		}

		// Property 2 above. Do not claim the next batch until this one has
		// finished writing its completion markers.
		wg.Wait()

		if ctx.Err() != nil {
			res.Interrupted = true
			break
		}
	}

	wg.Wait()
	return res
}
