package sqs_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	amazonsqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	smithymiddleware "github.com/aws/smithy-go/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-aws/sqs"
	"github.com/ThreeDotsLabs/watermill/message"
)

// TestDeleteBatch_EndToEnd verifies the batched-ack path against a real SQS
// API: every acked message is deleted (nothing left visible or in flight on
// the queue) using DeleteMessageBatch calls only — no per-message
// DeleteMessage calls.
func TestDeleteBatch_EndToEnd(t *testing.T) {
	t.Parallel()

	const messageCount = 25

	var (
		mu               sync.Mutex
		singleDeletes    int
		batchDeleteCalls int
		batchedEntries   int
	)
	countOps := func(o *amazonsqs.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *smithymiddleware.Stack) error {
			return stack.Initialize.Add(smithymiddleware.InitializeMiddlewareFunc("countDeletes",
				func(ctx context.Context, in smithymiddleware.InitializeInput, next smithymiddleware.InitializeHandler) (smithymiddleware.InitializeOutput, smithymiddleware.Metadata, error) {
					mu.Lock()
					switch awsmiddleware.GetOperationName(ctx) {
					case "DeleteMessage":
						singleDeletes++
					case "DeleteMessageBatch":
						batchDeleteCalls++
						if input, ok := in.Parameters.(*amazonsqs.DeleteMessageBatchInput); ok {
							batchedEntries += len(input.Entries)
						}
					}
					mu.Unlock()
					return next.HandleInitialize(ctx, in)
				}), smithymiddleware.After)
		})
	}

	cfg := newAwsConfig(t)
	logger := watermill.NewStdLogger(false, false)

	pub, err := sqs.NewPublisher(sqs.PublisherConfig{
		AWSConfig: cfg,
		OptFns:    []func(*amazonsqs.Options){GetEndpointResolverSqs()},
		Marshaler: sqs.DefaultMarshalerUnmarshaler{},
	}, logger)
	require.NoError(t, err)
	defer pub.Close()

	sub, err := sqs.NewSubscriber(sqs.SubscriberConfig{
		AWSConfig:      cfg,
		OptFns:         []func(*amazonsqs.Options){GetEndpointResolverSqs(), countOps},
		Unmarshaler:    sqs.DefaultMarshalerUnmarshaler{},
		ConsumeWorkers: 3,
		DeleteBatch: &sqs.DeleteBatchConfig{
			MaxSize: 10,
			Linger:  100 * time.Millisecond,
		},
	}, logger)
	require.NoError(t, err)

	topic := fmt.Sprintf("delete-batch-e2e-%s", watermill.NewUUID())
	require.NoError(t, sub.SubscribeInitialize(topic))
	for i := 0; i < messageCount; i++ {
		require.NoError(t, pub.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte("payload"))))
	}

	out, err := sub.Subscribe(context.Background(), topic)
	require.NoError(t, err)

	acked := make(chan struct{}, messageCount)
	go func() {
		for msg := range out {
			msg.Ack()
			acked <- struct{}{}
		}
	}()

	for i := 0; i < messageCount; i++ {
		select {
		case <-acked:
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for message %d/%d", i+1, messageCount)
		}
	}

	// Close flushes any pending batched deletes before returning.
	require.NoError(t, sub.Close())

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, singleDeletes, "batched mode must not issue per-message DeleteMessage calls")
	assert.Equal(t, messageCount, batchedEntries, "every acked message must be deleted exactly once")
	assert.Less(t, batchDeleteCalls, messageCount, "batching must issue fewer API calls than messages")

	// Nothing should be left on the queue: all deletes landed before Close returned.
	queueURL, err := sub.GetQueueUrl(context.Background(), topic)
	require.NoError(t, err)
	client := amazonsqs.NewFromConfig(cfg, GetEndpointResolverSqs())
	attrs, err := client.GetQueueAttributes(context.Background(), &amazonsqs.GetQueueAttributesInput{
		QueueUrl: aws.String(string(*queueURL)),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "0", attrs.Attributes["ApproximateNumberOfMessages"])
	assert.Equal(t, "0", attrs.Attributes["ApproximateNumberOfMessagesNotVisible"])
}
