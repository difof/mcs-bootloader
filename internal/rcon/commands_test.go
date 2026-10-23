package rcon

import (
	"fmt"
	"testing"
)

func TestNativePlayerCountsAcrossVersions(t *testing.T) {
	formats := []string{
		"There are %d/20 players online:%s",
		"There are %d of a max 20 players online: %s",
		"There are %d of a max of 20 players online: %s",
	}
	for _, format := range formats {
		for _, tc := range []struct {
			count int
			names string
			valid bool
		}{{0, "", true}, {2, "\nPlayer1, Player2", true}, {0, "unexpected", false}, {1, "", false}} {
			reply := fmt.Sprintf(format, tc.count, tc.names)
			count, err := parsePlayers(reply)
			if (err == nil) != tc.valid || (tc.valid && count != tc.count) {
				t.Errorf("%q: count=%d err=%v", reply, count, err)
			}
		}
	}
}
