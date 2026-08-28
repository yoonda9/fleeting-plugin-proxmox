package plugin

import (
	"context"
	"encoding/json"
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

	require.NoError(t, ig.Shutdown(context.Background()))

	// Call Shutdown twice more: with 1-capacity trigger channels, a second
	// blocking send would still fit in the empty buffer, so it takes a third
	// call to prove the fix rather than merely reuse leftover buffer space.
	for i := 0; i < 2; i++ {
		done := make(chan error, 1)

		go func() {
			done <- ig.Shutdown(context.Background())
		}()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatalf("Shutdown call #%d did not return: possible deadlock", i+2)
		}
	}
}
