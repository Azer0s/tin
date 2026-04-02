package main

import (
	"fmt"
	"time"
)

func runMPMC() {
	const n = 1_000_000
	const w = 4

	ch := make(chan int64, 64)
	done := make(chan struct{}, w)

	for i := 0; i < w; i++ {
		go func() {
			for j := 0; j < n/w; j++ {
				<-ch
			}
			done <- struct{}{}
		}()
	}

	start := time.Now()

	for i := 0; i < w; i++ {
		go func(id int) {
			for j := 0; j < n/w; j++ {
				ch <- int64(j)
			}
		}(i)
	}

	for i := 0; i < w; i++ {
		<-done
	}
	elapsed := time.Since(start)

	fmt.Printf("%d msgs, %dP+%dC\n", n, w, w)
	fmt.Printf("elapsed: ~%dms\n", elapsed.Milliseconds())
	fmt.Printf("throughput: ~%d msgs/sec\n", int64(float64(n)/elapsed.Seconds()))
}
