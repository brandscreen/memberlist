package memberlist

import (
	"log"
	"os"
	"testing"
	"time"
)

// CheckInteg will skip a test if integration testing is not enabled.
func CheckInteg(t *testing.T) {
	if !IsInteg() {
		t.SkipNow()
	}
}

// IsInteg returns a boolean telling you if we're in integ testing mode.
func IsInteg() bool {
	return os.Getenv("INTEG_TESTS") != ""
}

// Tests the memberlist by creating a cluster of 100 nodes
// and checking that we get strong convergence of changes.
func TestMemberlist_Integ(t *testing.T) {
	CheckInteg(t)

	num := 16
	var members []*Memberlist

	eventCh := make(chan NodeEvent, num)

	for i := 0; i < num; i++ {
		addr := getBindAddr()
		c := DefaultConfig()
		c.Name = addr.String()
		c.BindAddr = addr.String()
		c.ProbeInterval = 10 * time.Millisecond
		c.ProbeTimeout = 100 * time.Millisecond
		c.GossipInterval = 5 * time.Millisecond
		c.PushPullInterval = 100 * time.Millisecond

		if i == 0 {
			c.Events = &ChannelEventDelegate{eventCh}
		}

		m, err := Create(c)
		if err != nil {
			t.Fatalf("unexpected err: %s", err)
		}
		members = append(members, m)
		defer m.Shutdown()

		if i > 0 {
			last := members[i-1]
			num, err := m.Join([]string{last.config.Name})
			if num == 0 || err != nil {
				t.Fatalf("unexpected err: %s", err)
			}
		}
	}

	// Wait and print debug info
	breakTimer := time.After(250 * time.Millisecond)
WAIT:
	for {
		select {
		case e := <-eventCh:
			if e.Event == NodeJoin {
				log.Printf("[DEBUG] Node join: %v (%d)", *e.Node, members[0].NumMembers())
			} else {
				log.Printf("[DEBUG] Node leave: %v (%d)", *e.Node, members[0].NumMembers())
			}
		case <-breakTimer:
			break WAIT
		}
	}

	for idx, m := range members {
		got := m.NumMembers()
		if got != num {
			t.Errorf("bad num members at idx %d. Expected %d. Got %d.",
				idx, num, got)
		}
	}
}
