package sqs

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// sqsBatchDeleteAPI is the slice of *sqs.Client the batcher uses; an interface
// so tests can fake the transport.
type sqsBatchDeleteAPI interface {
	DeleteMessageBatch(ctx context.Context, params *sqs.DeleteMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error)
}

// deleteBatcher buffers receipt handles of acked messages for one queue and
// deletes them with DeleteMessageBatch, flushing when MaxSize handles are
// pending or on a Linger-based tick, whichever comes first. One batcher exists
// per Subscribe call; workers feed it concurrently via add.
type deleteBatcher struct {
	client   sqsBatchDeleteAPI
	queueURL QueueURL
	cfg      DeleteBatchConfig
	// flushTimeout bounds each DeleteMessageBatch call so a hung flush cannot
	// stall the flusher goroutine (or shutdown) indefinitely.
	flushTimeout time.Duration
	logger       watermill.LoggerAdapter

	mu      sync.Mutex
	pending []*string
	closed  bool

	kick chan struct{}
	done chan struct{}
	wg   sync.WaitGroup
}

func newDeleteBatcher(
	client sqsBatchDeleteAPI,
	queueURL QueueURL,
	cfg DeleteBatchConfig,
	flushTimeout time.Duration,
	logger watermill.LoggerAdapter,
) *deleteBatcher {
	b := &deleteBatcher{
		client:       client,
		queueURL:     queueURL,
		cfg:          cfg,
		flushTimeout: flushTimeout,
		logger:       logger.With(watermill.LogFields{"queue": queueURL}),
		kick:         make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
	b.wg.Add(1)
	go b.run()
	return b
}

// add buffers an acked message's receipt handle for batched deletion. Safe for
// concurrent use. If the batcher is already closed — only possible when an ack
// races subscriber shutdown — the handle is deleted immediately in its own
// call so an acked message is never left to redeliver silently.
func (b *deleteBatcher) add(receiptHandle *string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.deleteNow([]*string{receiptHandle})
		return
	}
	b.pending = append(b.pending, receiptHandle)
	full := len(b.pending) >= b.cfg.MaxSize
	b.mu.Unlock()

	if full {
		select {
		case b.kick <- struct{}{}:
		default: // a flush is already signaled
		}
	}
}

func (b *deleteBatcher) run() {
	defer b.wg.Done()

	ticker := time.NewTicker(b.cfg.Linger)
	defer ticker.Stop()

	for {
		select {
		case <-b.kick:
			b.flush()
		case <-ticker.C:
			b.flush()
		case <-b.done:
			b.flush()
			return
		}
	}
}

// flush drains everything currently pending, MaxSize handles per API call.
func (b *deleteBatcher) flush() {
	for {
		b.mu.Lock()
		n := len(b.pending)
		if n == 0 {
			b.mu.Unlock()
			return
		}
		if n > b.cfg.MaxSize {
			n = b.cfg.MaxSize
		}
		batch := make([]*string, n)
		copy(batch, b.pending[:n])
		b.pending = b.pending[n:]
		b.mu.Unlock()

		b.deleteNow(batch)
	}
}

func (b *deleteBatcher) deleteNow(handles []*string) {
	// Deliberately detached from the receive/close contexts: acked work must
	// be deleted even while the subscriber is shutting down.
	ctx, cancel := context.WithTimeout(context.Background(), b.flushTimeout)
	defer cancel()

	entries := make([]types.DeleteMessageBatchRequestEntry, len(handles))
	for i, h := range handles {
		entries[i] = types.DeleteMessageBatchRequestEntry{
			Id:            aws.String(strconv.Itoa(i)),
			ReceiptHandle: h,
		}
	}

	out, err := b.client.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{
		QueueUrl: aws.String(string(b.queueURL)),
		Entries:  entries,
	})
	if err != nil {
		// The messages stay in flight and SQS redelivers them after the
		// visibility timeout — the same at-least-once outcome as a failed
		// synchronous delete.
		b.logger.Error("Cannot batch-delete acked messages, they will be redelivered", err, watermill.LogFields{
			"count": len(handles),
		})
		return
	}
	for _, f := range out.Failed {
		b.logger.Error("Failed to delete acked message in batch, it will be redelivered",
			fmt.Errorf("%s: %s", aws.ToString(f.Code), aws.ToString(f.Message)), nil)
	}
}

// close flushes everything pending and stops the flusher. Idempotent. Adds
// racing close fall back to immediate single-entry deletes (see add), so no
// acked handle is dropped.
func (b *deleteBatcher) close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.wg.Wait()
		return
	}
	b.closed = true
	b.mu.Unlock()

	close(b.done)
	b.wg.Wait()
}
