package main

import (
	"fmt"
	"time"
)

// process_one mirrors the Tin/Crystal/Rust workload's per-item work:
// a few string allocations + an integer hash-mix so the loop body
// can't be optimized away to a no-op.
func processOne(v int64) int64 {
	key := fmt.Sprintf("item-%d", v)
	header := "[req] " + key
	trailer := key + " :ok"
	combo := header + " | " + trailer
	m := int64(len(combo))

	return ((v * 1315423911) ^ (m * 2654435761)) & 0x7fffffff
}

func runWorkload() {
	const n = 200_000
	const workers = 8

	in := make(chan int64, 64)
	out := make(chan int64, 64)

	for i := 0; i < workers; i++ {
		go func() {
			for v := range in {
				if v < 0 {
					return
				}
				out <- processOne(v)
			}
		}()
	}

	start := time.Now()

	// Producer goroutine: feeds `in` without blocking the main drain.
	go func() {
		for i := int64(0); i < n; i++ {
			in <- i
		}
	}()

	var acc int64
	for i := 0; i < n; i++ {
		acc += <-out
	}

	elapsed := time.Since(start)

	for i := 0; i < workers; i++ {
		in <- -1
	}

	fmt.Printf("%d items, %d workers, acc=%d\n", n, workers, acc)
	fmt.Printf("elapsed: ~%dms\n", elapsed.Milliseconds())
	fmt.Printf("throughput: ~%d items/sec\n", int64(float64(n)/elapsed.Seconds()))
}
