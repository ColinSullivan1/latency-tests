package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/codahale/hdrhistogram"
	"github.com/nats-io/go-nats"
	"github.com/tylertreat/hdrhistogram-writer"
)

var (
	ServerA, ServerB string
	TargetPubRate    int
	MsgSize          int
	NumPubs          int
	TestDuration     time.Duration
	HistFile         string
)

func usage() {
	log.Fatalf("Usage: latency [-sa serverA] [-sb serverB] [-sz msgSize] [-tr msgs/sec] [-tt testTime] [-hist <file>]\n")
}

func main() {
	flag.StringVar(&ServerA, "sa", nats.DefaultURL, "ServerA - Publisher")
	flag.StringVar(&ServerB, "sb", nats.DefaultURL, "ServerB - Subscriber")
	flag.IntVar(&TargetPubRate, "tr", 1000, "Target Publish Rate")
	flag.IntVar(&MsgSize, "sz", 8, "Message Payload Size")
	flag.DurationVar(&TestDuration, "tt", 5, "Target Test Time")
	flag.StringVar(&HistFile, "hist", "", "Histogram Output")

	log.SetFlags(0)
	flag.Usage = usage
	flag.Parse()

	NumPubs = int(TestDuration/time.Second) * TargetPubRate

	if MsgSize < 8 {
		log.Fatalf("Message Payload Size must be at least %d bytes\n", 8)
	}

	c1, err := nats.Connect(ServerA)
	if err != nil {
		log.Fatalf("Could not connect to ServerA: %v", err)
	}
	c2, err := nats.Connect(ServerB)
	if err != nil {
		log.Fatalf("Could not connect to ServerA: %v", err)
	}

	// Do some qiuck RTT calculations
	log.Println("==============================")
	now := time.Now()
	c1.Flush()
	log.Printf("Pub Server RTT : %v\n", fmtDur(time.Since(now)))

	now = time.Now()
	c2.Flush()
	log.Printf("Sub Server RTT : %v\n", fmtDur(time.Since(now)))

	// Duration tracking
	durations := make([]time.Duration, 0, NumPubs)

	// Wait for all messages to be received.
	var wg sync.WaitGroup
	wg.Add(1)

	//Random subject (to run multiple tests in parallel)
	subject := nats.NewInbox()

	// Count the messages.
	received := 0

	// Async Subscriber (Runs in its own Goroutine)
	c2.Subscribe(subject, func(msg *nats.Msg) {
		sendTime := int64(binary.LittleEndian.Uint64(msg.Data))
		durations = append(durations, time.Duration(time.Now().UnixNano()-sendTime))
		received++
		if received >= NumPubs {
			wg.Done()
		}
	})
	// Make sure interest is set for subscribe before publish since a different connection.
	c2.Flush()

	log.Printf("Message Payload: %v\n", byteSize(MsgSize))
	log.Printf("Target Duration: %v\n", TestDuration)
	log.Printf("Target Msgs/Sec: %v\n", TargetPubRate)
	log.Printf("Target Band/Sec: %v\n", byteSize(TargetPubRate*MsgSize*2))
	log.Println("==============================")

	// Random payload
	data := make([]byte, MsgSize)
	io.ReadFull(rand.Reader, data)

	// For publish throttling
	delay := time.Second / time.Duration(TargetPubRate)
	start := time.Now()

	// Throttle logic, crude I know, but works better then time.Ticker.
	adjustAndSleep := func(count int) {
		r := rps(count, time.Since(start))
		adj := delay / 20 // 5%
		if adj == 0 {
			adj = 1 // 1ns min
		}
		if r < TargetPubRate {
			delay -= adj
		} else if r > TargetPubRate {
			delay += adj
		}
		if delay < 0 {
			delay = 0
		}
		time.Sleep(delay)
	}

	// Now publish
	for i := 0; i < NumPubs; i++ {
		now := time.Now()
		// Place the send time in the front of the payload.
		binary.LittleEndian.PutUint64(data[0:], uint64(now.UnixNano()))
		c1.Publish(subject, data)
		adjustAndSleep(i + 1)
	}
	pubDur := time.Since(start)
	wg.Wait()

	// Print results
	log.Printf("Actual Msgs/Sec: %d\n", rps(NumPubs, pubDur))
	log.Printf("Actual Band/Sec: %v\n", byteSize(rps(NumPubs, pubDur)*MsgSize*2))
	log.Printf("Actual Duration: %v\n", fmtDur(time.Since(start)))

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	h := hdrhistogram.New(1, int64(durations[len(durations)-1]), 5)
	for _, d := range durations {
		h.RecordValue(int64(d))
	}
	log.Printf("HDR Percentiles:\n10:\t%v\n50:\t%v\n75:\t%v\n90:\t%v\n99:\t%v\n99.99:\t%v\n",
		fmtDur(time.Duration(h.ValueAtQuantile(10))),
		fmtDur(time.Duration(h.ValueAtQuantile(50))),
		fmtDur(time.Duration(h.ValueAtQuantile(75))),
		fmtDur(time.Duration(h.ValueAtQuantile(90))),
		fmtDur(time.Duration(h.ValueAtQuantile(99))),
		fmtDur(time.Duration(h.ValueAtQuantile(99.99))))
	log.Println("==============================")

	if HistFile != "" {
		pctls := histwriter.Percentiles{10, 25, 50, 75, 90, 99, 99.9, 99.99, 99.999}
		histwriter.WriteDistributionFile(h, pctls, 1.0/1000000.0, HistFile+".histogram")
	}
}

const fsecs = float64(time.Second)

func rps(count int, elapsed time.Duration) int {
	return int(float64(count) / (float64(elapsed) / fsecs))
}

// Just pretty print the byte sizes.
func byteSize(n int) string {
	sizes := []string{"B", "K", "M", "G", "T"}
	base := float64(1024)
	if n < 10 {
		return fmt.Sprintf("%d%s", n, sizes[0])
	}
	e := math.Floor(logn(float64(n), base))
	suffix := sizes[int(e)]
	val := math.Floor(float64(n)/math.Pow(base, e)*10+0.5) / 10
	f := "%.0f%s"
	if val < 10 {
		f = "%.1f%s"
	}
	return fmt.Sprintf(f, val, suffix)
}

func logn(n, b float64) float64 {
	return math.Log(n) / math.Log(b)
}

// Make time durations a bit prettier.
func fmtDur(t time.Duration) time.Duration {
	if t > time.Microsecond && t < time.Millisecond {
		return t.Truncate(time.Microsecond)
	}
	return t.Truncate(time.Millisecond)
}
