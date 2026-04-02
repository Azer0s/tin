package main

import (
	"fmt"
	"time"
)

func relay(in, out chan int64, n int, done chan struct{}) {
	for i := 0; i < n; i++ {
		v := <-in
		out <- v + 1
	}
	close(done)
}

func runPipeline() {
	const n = 1_000_000
	const stages = 4

	ch := make([]chan int64, stages+1)
	for i := range ch {
		ch[i] = make(chan int64, 1)
	}

	dones := make([]chan struct{}, stages)
	for i := 0; i < stages; i++ {
		dones[i] = make(chan struct{})
		go relay(ch[i], ch[i+1], n, dones[i])
	}

	start := time.Now()
	for i := 0; i < n; i++ {
		ch[0] <- int64(i)
		<-ch[stages]
	}
	elapsed := time.Since(start)

	for _, d := range dones {
		<-d
	}

	fmt.Printf("%d messages through %d stages\n", n, stages)
	fmt.Printf("elapsed: ~%dms\n", elapsed.Milliseconds())
	fmt.Printf("latency: ~%dns / pipeline pass\n", elapsed.Nanoseconds()/n)
}
