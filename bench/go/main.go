package main

import (
	"fmt"
	"os"
	"time"
)

func runPingPong() {
	const n = 1_000_000
	ping := make(chan int64, 1)
	pong := make(chan int64, 1)

	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			v := <-ping
			pong <- v
		}
		close(done)
	}()

	start := time.Now()
	for i := 0; i < n; i++ {
		ping <- 1
		<-pong
	}
	elapsed := time.Since(start)
	<-done

	fmt.Printf("%d round trips\n", n)
	fmt.Printf("elapsed: ~%dms\n", elapsed.Milliseconds())
	fmt.Printf("latency: ~%dns / round trip\n", elapsed.Nanoseconds()/n)
}

func main() {
	bench := "pingpong"
	if len(os.Args) > 1 {
		bench = os.Args[1]
	}
	switch bench {
	case "pipeline":
		runPipeline()
	case "mpmc":
		runMPMC()
	case "jitter":
		runJitter()
	case "pipeline10":
		runPipeline10()
	case "fanout":
		runFanout()
	default:
		runPingPong()
	}
}
