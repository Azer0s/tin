package main

import (
	"fmt"
	"runtime"
	"time"
)

func runJitter() {
	const n = 1_000_000
	const w = 8

	tasks := make(chan int64, 256)
	results := make(chan struct{}, 256)

	for i := 0; i < w; i++ {
		go func() {
			for cost := range tasks {
				for j := int64(0); j < cost; j++ {
					runtime.Gosched()
				}
				results <- struct{}{}
			}
		}()
	}

	start := time.Now()

	go func() {
		for i := int64(0); i < n; i++ {
			tasks <- i % 4
		}
		close(tasks)
	}()

	for i := 0; i < n; i++ {
		<-results
	}
	elapsed := time.Since(start)

	fmt.Printf("%d tasks, %d workers, cost 0-3 yields/task\n", n, w)
	fmt.Printf("elapsed: ~%dms\n", elapsed.Milliseconds())
	fmt.Printf("throughput: ~%d tasks/sec\n", int64(float64(n)/elapsed.Seconds()))
}
