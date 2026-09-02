package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUnmarshallingPluginSettings(t *testing.T) {
	settingsJSON := `{"url":"sample_url","template_id": 5}`
	instance := InstanceGroup{}

	err := json.Unmarshal([]byte(settingsJSON), &instance)
	require.NoError(t, err)

	require.Equal(t, "sample_url", instance.URL)
	require.Equal(t, 5, *instance.TemplateID)
}

func TestShutdownIsIdempotent(t *testing.T) {
	ig := &InstanceGroup{
		collectorShutdownTrigger:              make(chan struct{}, 1),
		sessionTicketRefresherShutdownTrigger: make(chan struct{}, 1),
	}

	ig.collectorWaitGroup.Go(func() {
		<-ig.collectorShutdownTrigger
	})

	ig.sessionTicketRefresherWaitGroup.Go(func() {
		<-ig.sessionTicketRefresherShutdownTrigger
	})

	// Shutdown must return every time, not just the first.
	for i := range 3 {
		done := make(chan error, 1)

		go func() {
			done <- ig.Shutdown(context.Background())
		}()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatalf("Shutdown call #%d did not return: possible deadlock", i+1)
		}
	}
}

func TestShutdownWithoutInitDoesNotPanic(t *testing.T) {
	// A group whose Init never ran still has nil trigger channels, and closing a nil channel
	// panics.
	ig := &InstanceGroup{}

	require.NotPanics(t, func() {
		require.NoError(t, ig.Shutdown(context.Background()))
	})
}

func TestShutdownAfterInitCycleDoesNotHang(t *testing.T) {
	ig := &InstanceGroup{}

	// A Shutdown before the first Init has nothing to close, and resetLifecycle re-arms the
	// guard on every Init, so what that first call does with the guard cannot strand a later
	// cycle. The requirement is only that every Shutdown here returns.
	requireShutdownReturns(t, ig, "before the first Init")

	for cycle := range 2 {
		ig.resetLifecycle()
		startFakeWorkers(ig)
		requireShutdownReturns(t, ig, fmt.Sprintf("after Init cycle #%d", cycle+1))
	}
}

// requireShutdownReturns fails the test instead of letting the package time out when Shutdown
// blocks -- a Shutdown that waits on workers nothing ever told to stop never returns at all.
func requireShutdownReturns(t *testing.T, ig *InstanceGroup, when string) {
	t.Helper()

	done := make(chan error, 1)

	go func() {
		done <- ig.Shutdown(context.Background())
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatalf("Shutdown %s did not return: possible deadlock", when)
	}
}

// startFakeWorkers stands in for the collector and the session ticket refresher: each returns
// once its shutdown trigger is closed, which is all that Shutdown's waits depend on.
func startFakeWorkers(ig *InstanceGroup) {
	ig.collectorWaitGroup.Go(func() {
		<-ig.collectorShutdownTrigger
	})

	ig.sessionTicketRefresherWaitGroup.Go(func() {
		<-ig.sessionTicketRefresherShutdownTrigger
	})
}
