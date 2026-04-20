package main

import (
	"fmt"
	"time"
)

func runFanout() {
	const n = 1_000_000
	const workers = 8

	in := make(chan int64, 64)
	out := make(chan int64, 64)

	for i := 0; i < workers; i++ {
		go func() {
			for v := range in {
				if v < 0 {
					return
				}
				out <- v * 2
			}
		}()
	}

	start := time.Now()
	for i := int64(0); i < n; i++ {
		in <- i
		<-out
	}
	elapsed := time.Since(start)

	for i := 0; i < workers; i++ {
		in <- -1
	}

	fmt.Printf("%d items, %d workers\n", n, workers)
	fmt.Printf("elapsed: ~%dms\n", elapsed.Milliseconds())
	fmt.Printf("throughput: ~%d items/sec\n", int64(float64(n)/elapsed.Seconds()))
}
