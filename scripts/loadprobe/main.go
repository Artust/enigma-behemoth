// Command loadprobe is a dependency-free load generator for POST /damage.
// It measures throughput and latency percentiles, and asserts p99 < 100ms.
//
// Use it when k6 is not installed:
//
//	go run ./scripts/loadprobe
//	BASE_URL=http://localhost:8080 BOSS_ID=boss-1 go run ./scripts/loadprobe
//
// Note: it drains and reuses HTTP keep-alive connections — measuring the server,
// not TCP connection setup.
package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	base := getenv("BASE_URL", "http://localhost:8080")
	boss := getenv("BOSS_ID", "boss-1")
	concurrency := 200
	duration := 15 * time.Second

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 500,
			MaxConnsPerHost:     500,
		},
	}

	var (
		ok, transportErr, s503, s5xx int64
		mu                           sync.Mutex
		lats                         []time.Duration
		sampleErr                    atomic.Value
	)
	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed)))
			local := make([]time.Duration, 0, 4096)
			for time.Now().Before(deadline) {
				body := fmt.Sprintf(`{"player_id":"p-%d","boss_id":%q,"damage_amount":1}`, rng.Intn(20000), boss)
				start := time.Now()
				resp, err := client.Post(base+"/damage", "application/json", bytes.NewBufferString(body))
				el := time.Since(start)
				if err != nil {
					atomic.AddInt64(&transportErr, 1)
					sampleErr.Store(err.Error())
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				switch {
				case resp.StatusCode == 503:
					atomic.AddInt64(&s503, 1)
				case resp.StatusCode >= 500:
					atomic.AddInt64(&s5xx, 1)
				default:
					atomic.AddInt64(&ok, 1)
					local = append(local, el)
				}
			}
			mu.Lock()
			lats = append(lats, local...)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	pct := func(p float64) time.Duration {
		if len(lats) == 0 {
			return 0
		}
		idx := int(p / 100 * float64(len(lats)))
		if idx >= len(lats) {
			idx = len(lats) - 1
		}
		return lats[idx]
	}
	fail := transportErr + s503 + s5xx
	fmt.Printf("target=%s boss=%s concurrency=%d duration=%s\n", base, boss, concurrency, duration)
	fmt.Printf("requests: total=%d ok=%d fail=%d (transport=%d http503=%d http5xx=%d)\n",
		ok+fail, ok, fail, transportErr, s503, s5xx)
	if e := sampleErr.Load(); e != nil {
		fmt.Printf("sample transport error: %v\n", e)
	}
	fmt.Printf("throughput: %.0f QPS (ok)\n", float64(ok)/duration.Seconds())
	fmt.Printf("latency: p50=%v p90=%v p99=%v p999=%v max=%v\n",
		pct(50), pct(90), pct(99), pct(99.9), pct(100))
	if pct(99) < 100*time.Millisecond {
		fmt.Println("RESULT: PASS (p99 < 100ms)")
		return
	}
	fmt.Println("RESULT: FAIL (p99 >= 100ms)")
	os.Exit(1)
}
