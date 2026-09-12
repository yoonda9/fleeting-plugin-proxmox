package plugin

import (
	"testing"
	"time"
)

func TestTriggerCollectionOnFullChannelReturnsImmediately(t *testing.T) {
	ig := &InstanceGroup{
		instanceCollectionTrigger: make(chan struct{}, 1),
	}

	// Fill the channel so a blocking send would stall forever.
	ig.instanceCollectionTrigger <- struct{}{}

	done := make(chan struct{})

	go func() {
		ig.triggerCollection()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("triggerCollection blocked on a full channel")
	}
}
