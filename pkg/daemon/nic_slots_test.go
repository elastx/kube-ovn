package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAcquireNicSlotUncapped(t *testing.T) {
	csh := createCniServerHandler(&Configuration{}, nil)
	require.Nil(t, csh.nicSlots)
	for range 100 {
		release, err := csh.acquireNicSlot(context.Background())
		require.NoError(t, err)
		defer release()
	}
}

func TestAcquireNicSlotCapsAndQueues(t *testing.T) {
	csh := createCniServerHandler(&Configuration{MaxConcurrentNic: 2}, nil)

	r1, err := csh.acquireNicSlot(context.Background())
	require.NoError(t, err)
	r2, err := csh.acquireNicSlot(context.Background())
	require.NoError(t, err)

	// The third call queues while both slots are held.
	got := make(chan func(), 1)
	go func() {
		r3, err := csh.acquireNicSlot(context.Background())
		if err == nil {
			got <- r3
		}
	}()
	select {
	case <-got:
		t.Fatal("a third configure-nic ran with a cap of 2")
	case <-time.After(100 * time.Millisecond):
	}

	// Freeing one slot lets it through.
	r1()
	select {
	case r3 := <-got:
		r3()
	case <-time.After(2 * time.Second):
		t.Fatal("a queued configure-nic did not start after a slot was released")
	}
	r2()
	require.Empty(t, csh.nicSlots)
}

func TestAcquireNicSlotGivesUpWithTheRequest(t *testing.T) {
	csh := createCniServerHandler(&Configuration{MaxConcurrentNic: 1}, nil)
	release, err := csh.acquireNicSlot(context.Background())
	require.NoError(t, err)
	defer release()

	// The CNI caller went away (kubelet's runtime request timed out): the
	// queued call fails instead of holding a slot nobody waits for.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = csh.acquireNicSlot(ctx)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	require.ErrorContains(t, err, "configure-nic slot (1 in use)")
	require.Len(t, csh.nicSlots, 1)
}

func TestAcquireNicSlotQueueTimeout(t *testing.T) {
	// A slot that never frees: the queued ADD fails after the queue bound,
	// even though its caller is still waiting.
	csh := createCniServerHandler(&Configuration{MaxConcurrentNic: 1, NicQueueTimeout: 50 * time.Millisecond}, nil)
	release, err := csh.acquireNicSlot(context.Background())
	require.NoError(t, err)
	defer release()

	start := time.Now()
	_, err = csh.acquireNicSlot(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}
