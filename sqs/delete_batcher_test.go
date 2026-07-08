package sqs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/aws/aws-sdk-go-v2/aws"
	amazonsqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeBatchDeleteClient struct {
	mu    sync.Mutex
	calls [][]types.DeleteMessageBatchRequestEntry
}

func (f *fakeBatchDeleteClient) DeleteMessageBatch(_ context.Context, params *amazonsqs.DeleteMessageBatchInput, _ ...func(*amazonsqs.Options)) (*amazonsqs.DeleteMessageBatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, params.Entries)
	return &amazonsqs.DeleteMessageBatchOutput{}, nil
}

func (f *fakeBatchDeleteClient) snapshot() (calls int, handles []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		for _, e := range c {
			handles = append(handles, aws.ToString(e.ReceiptHandle))
		}
	}
	return len(f.calls), handles
}

func newTestBatcher(client sqsBatchDeleteAPI, cfg DeleteBatchConfig) *deleteBatcher {
	cfg.setDefaults()
	return newDeleteBatcher(client, "http://queue.test/q", cfg, 5*time.Second, watermill.NopLogger{})
}

func handlePtrs(n int) []*string {
	out := make([]*string, n)
	for i := range out {
		h := string(rune('a'+i%26)) + "-" + watermill.NewShortUUID()
		out[i] = &h
	}
	return out
}

// 25 rapid acks must arrive in DeleteMessageBatch calls of at most 10 entries,
// with every receipt handle delivered exactly once.
func TestDeleteBatcher_SizeTriggeredFlush(t *testing.T) {
	t.Parallel()

	client := &fakeBatchDeleteClient{}
	b := newTestBatcher(client, DeleteBatchConfig{MaxSize: 10, Linger: time.Hour})

	handles := handlePtrs(25)
	for _, h := range handles {
		b.add(h)
	}
	b.close()

	calls, got := client.snapshot()
	require.Len(t, got, 25, "every acked handle must be deleted exactly once")
	assert.LessOrEqual(t, calls, 4, "25 handles should need at most 3 full batches plus a remainder")
	want := map[string]bool{}
	for _, h := range handles {
		want[*h] = true
	}
	for _, h := range got {
		assert.True(t, want[h], "unexpected handle deleted: %s", h)
	}
}

// A partial batch must flush on the linger tick without reaching MaxSize.
func TestDeleteBatcher_LingerTriggeredFlush(t *testing.T) {
	t.Parallel()

	client := &fakeBatchDeleteClient{}
	b := newTestBatcher(client, DeleteBatchConfig{MaxSize: 10, Linger: 50 * time.Millisecond})
	defer b.close()

	for _, h := range handlePtrs(3) {
		b.add(h)
	}

	require.Eventually(t, func() bool {
		_, got := client.snapshot()
		return len(got) == 3
	}, 2*time.Second, 10*time.Millisecond, "linger tick should flush the partial batch")
}

// close must flush whatever is pending before returning.
func TestDeleteBatcher_CloseFlushesPending(t *testing.T) {
	t.Parallel()

	client := &fakeBatchDeleteClient{}
	b := newTestBatcher(client, DeleteBatchConfig{MaxSize: 10, Linger: time.Hour})

	for _, h := range handlePtrs(4) {
		b.add(h)
	}
	b.close()

	_, got := client.snapshot()
	assert.Len(t, got, 4)
}

// An ack racing shutdown (add after close) must still delete, immediately.
func TestDeleteBatcher_AddAfterCloseDeletesImmediately(t *testing.T) {
	t.Parallel()

	client := &fakeBatchDeleteClient{}
	b := newTestBatcher(client, DeleteBatchConfig{MaxSize: 10, Linger: time.Hour})
	b.close()

	h := "late-ack"
	b.add(&h)

	_, got := client.snapshot()
	require.Equal(t, []string{"late-ack"}, got)
}

func TestDeleteBatchConfig_Defaults(t *testing.T) {
	t.Parallel()

	c := DeleteBatchConfig{}
	c.setDefaults()
	assert.Equal(t, 10, c.MaxSize)
	assert.Equal(t, 500*time.Millisecond, c.Linger)

	c = DeleteBatchConfig{MaxSize: 50, Linger: time.Second}
	c.setDefaults()
	assert.Equal(t, 10, c.MaxSize, "MaxSize above the SQS limit must clamp to 10")
	assert.Equal(t, time.Second, c.Linger)
}
