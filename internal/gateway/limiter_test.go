package gateway

import (
	"fmt"
	"testing"
	"time"
)

func TestWakeLimiterAllowsRetriesWithoutUnboundedState(t *testing.T) {
	var limiter wakeLimiter
	now := time.Unix(100, 0)
	for i := 0; i < 3; i++ {
		if !limiter.allow("friend", now) {
			t.Fatal("initial connection burst blocked")
		}
	}
	if limiter.allow("friend", now) {
		t.Fatal("unbounded burst allowed")
	}
	if !limiter.allow("friend", now.Add(5*time.Second)) {
		t.Fatal("retry did not refill")
	}
	for i := 0; i < 2000; i++ {
		limiter.allow(fmt.Sprintf("scan-%d", i), now.Add(10*time.Second))
	}
	if len(limiter.byIP) > 1024 {
		t.Fatalf("unbounded limiter map: %d", len(limiter.byIP))
	}
	if !limiter.allow("new-friend", now.Add(time.Minute)) {
		t.Fatal("new legitimate address permanently blocked")
	}
}
