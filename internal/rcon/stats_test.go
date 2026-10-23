package rcon

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestStatisticsNeverRetainCommandsCredentialsOrReplies(t *testing.T) {
	c := New("unused", "credential-never-in-metrics", time.Second)
	_, _ = c.CommandWithTimeout(context.Background(), "say secret-operator-text", 0)
	_, _ = c.CommandWithTimeout(context.Background(), "data get entity Alice Health", 0)
	stats := c.Stats()
	text := fmt.Sprintf("%+v", stats)
	for _, secret := range []string{"credential-never-in-metrics", "secret-operator-text", "Alice", "Health"} {
		if strings.Contains(text, secret) {
			t.Fatalf("statistics leaked %q", secret)
		}
	}
	var requests uint64
	for _, op := range stats.Operations {
		requests += op.Requests
		if op.Requests != op.Errors+op.Successes {
			t.Fatal("inconsistent request totals")
		}
		for i, n := range op.DurationBuckets {
			if n > op.Requests || (i > 0 && n < op.DurationBuckets[i-1]) {
				t.Fatal("invalid histogram buckets")
			}
		}
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
	next := c.Replacement("new-secret", time.Second)
	if fmt.Sprintf("%+v", next.Stats()) != text {
		t.Fatal("password rotation reset aggregate statistics")
	}
}

func TestStatsSuccessfulExchangeAndAuthenticationFailure(t *testing.T) {
	c := fakeRCON(t, func(conn net.Conn) error {
		if err := authenticate(conn, false); err != nil {
			return err
		}
		if err := readRequest(conn, "list"); err != nil {
			return err
		}
		return finish(conn, "There are 0 of a max of 5 players online: ")
	})
	if _, err := c.Players(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.Connections != 1 || s.AuthenticationSuccesses != 1 || s.AuthenticationErrors != 0 || s.Connected || s.InFlight != 0 {
		t.Fatalf("incorrect transport stats %+v", s)
	}
	if s.Operations[0].Successes != 1 || s.Operations[0].ResponseBytes == 0 {
		t.Fatal("missing successful command observation")
	}
	bad := fakeRCON(t, func(conn net.Conn) error {
		if _, err := readPacket(conn); err != nil {
			return err
		}
		return writePacket(conn, -1, 2, "")
	})
	if _, err := bad.Command(context.Background(), "list"); err == nil {
		t.Fatal("authentication unexpectedly succeeded")
	}
	s = bad.Stats()
	if s.AuthenticationErrors != 1 || s.Operations[0].Errors != 1 {
		t.Fatalf("authentication failure not counted %+v", s)
	}
}
