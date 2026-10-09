//go:build ignore

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type LatencyResult struct {
	Duration time.Duration
	Status   int
	Err      error
}

func main() {
	baseURL := flag.String("base", "http://localhost:8080", "Base URL of the server")
	concurrency := flag.Int("concurrency", 50, "Concurrent worker routines")
	duration := flag.Duration("duration", 10*time.Second, "Duration of the load test")
	targetRPS := flag.Int("rps", 1000, "Target requests per second across all workers")
	endpoint := flag.String("endpoint", "/healthz", "Endpoint path to load test")
	flag.Parse()

	targetURL := *baseURL + *endpoint
	fmt.Printf("=== PG-GO High-Throughput Load Benchmark ===\n")
	fmt.Printf("Target URL:    %s\n", targetURL)
	fmt.Printf("Concurrency:   %d workers\n", *concurrency)
	fmt.Printf("Duration:      %v\n", *duration)
	fmt.Printf("Target Rate:   %d req/sec\n\n", *targetRPS)

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 500,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	var (
		totalRequests uint64
		successCount  uint64
		errorCount    uint64
		mu            sync.Mutex
		latencies     []time.Duration
	)

	// Pre-allocate approximate slice for latencies
	latencies = make([]time.Duration, 0, *targetRPS*int(duration.Seconds()))

	start := time.Now()
	var wg sync.WaitGroup
	intervalPerWorker := time.Duration(float64(time.Second) * float64(*concurrency) / float64(*targetRPS))

	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			ticker := time.NewTicker(intervalPerWorker)
			defer ticker.Stop()

			localLatencies := make([]time.Duration, 0, 1000)

			for {
				select {
				case <-ctx.Done():
					mu.Lock()
					latencies = append(latencies, localLatencies...)
					mu.Unlock()
					return
				case <-ticker.C:
					reqStart := time.Now()
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
					if err != nil {
						atomic.AddUint64(&errorCount, 1)
						continue
					}

					resp, err := client.Do(req)
					reqDuration := time.Since(reqStart)
					atomic.AddUint64(&totalRequests, 1)

					if err != nil {
						atomic.AddUint64(&errorCount, 1)
					} else {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						if resp.StatusCode >= 200 && resp.StatusCode < 400 {
							atomic.AddUint64(&successCount, 1)
						} else {
							atomic.AddUint64(&errorCount, 1)
						}
					}
					localLatencies = append(localLatencies, reqDuration)
				}
			}
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start)

	if len(latencies) == 0 {
		fmt.Printf("No requests completed successfully.\n")
		os.Exit(1)
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	var sumNs int64
	for _, d := range latencies {
		sumNs += d.Nanoseconds()
	}
	avg := time.Duration(sumNs / int64(len(latencies)))
	p50 := latencies[int(math.Min(float64(len(latencies)-1), float64(len(latencies))*0.50))]
	p95 := latencies[int(math.Min(float64(len(latencies)-1), float64(len(latencies))*0.95))]
	p99 := latencies[int(math.Min(float64(len(latencies)-1), float64(len(latencies))*0.99))]
	minDur := latencies[0]
	maxDur := latencies[len(latencies)-1]

	actualRPS := float64(totalRequests) / elapsed.Seconds()

	fmt.Printf("=== Benchmark Results ===\n")
	fmt.Printf("Elapsed Time:    %v\n", elapsed)
	fmt.Printf("Total Completed: %d requests\n", totalRequests)
	fmt.Printf("Successful (2xx):%d\n", successCount)
	fmt.Printf("Failed:          %d\n", errorCount)
	fmt.Printf("Actual Rate:     %.2f req/sec\n\n", actualRPS)
	fmt.Printf("Latency Distribution:\n")
	fmt.Printf("  Min:           %v\n", minDur)
	fmt.Printf("  Avg:           %v\n", avg)
	fmt.Printf("  p50 (Median):  %v\n", p50)
	fmt.Printf("  p95:           %v\n", p95)
	fmt.Printf("  p99:           %v\n", p99)
	fmt.Printf("  Max:           %v\n", maxDur)
	fmt.Printf("=========================\n")
}
