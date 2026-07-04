package store

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestRedisStarting(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"loading", errors.New("LOADING Redis is loading the dataset in memory"), true},
		{"refused", errors.New("dial tcp 127.0.0.1:6379: connect: connection refused"), true},
		{"bad auth", errors.New("WRONGPASS invalid username-password pair"), false},
		{"no such host", errors.New("dial tcp: lookup nope: no such host"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redisStarting(tt.err); got != tt.want {
				t.Errorf("redisStarting(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestNewRedisStore_RetriesUntilDeadline: refused connection is transient, retries
// until ctx expires, rather than the old fail-on-first-ping behavior that crash-exited.
func TestNewRedisStore_RetriesUntilDeadline(t *testing.T) {
	// Closed listener gives an address that reliably refuses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	const budget = 400 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_, err = NewRedisStore(ctx, addr, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("NewRedisStore against a closed port = nil error, want failure after retries")
	}
	// Must have retried across the budget, not returned on first refusal.
	if elapsed < budget/2 {
		t.Fatalf("returned after %v, want it to keep retrying for ~%v (did not treat refusal as transient)", elapsed, budget)
	}
}
