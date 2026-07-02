package sqs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	amazonsqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-aws/sqs"
	"github.com/ThreeDotsLabs/watermill/message"
)

// TestDeadLetterQueue_ProvisionsQueueAndRedrivePolicy verifies that a subscriber
// configured with DeadLetterQueue creates the companion "-dlq" queue and
// attaches a redrive policy on the source queue pointing at the DLQ's ARN with
// the configured MaxReceiveCount.
func TestDeadLetterQueue_ProvisionsQueueAndRedrivePolicy(t *testing.T) {
	t.Parallel()

	cfg := newAwsConfig(t)

	sub, err := sqs.NewSubscriber(sqs.SubscriberConfig{
		AWSConfig:       cfg,
		OptFns:          []func(*amazonsqs.Options){GetEndpointResolverSqs()},
		Unmarshaler:     sqs.DefaultMarshalerUnmarshaler{},
		DeadLetterQueue: &sqs.DeadLetterQueueConfig{MaxReceiveCount: 3},
	}, watermill.NewStdLogger(false, false))
	require.NoError(t, err)
	defer func() { _ = sub.Close() }()

	topic := fmt.Sprintf("dlq-provision-%s", watermill.NewUUID())
	require.NoError(t, sub.SubscribeInitialize(topic))

	client := amazonsqs.NewFromConfig(cfg, GetEndpointResolverSqs())

	// The DLQ was created and has an ARN.
	dlqURL := mustQueueURL(t, client, topic+"-dlq")
	dlqArn := mustQueueAttribute(t, client, dlqURL, types.QueueAttributeNameQueueArn)
	require.NotEmpty(t, dlqArn)

	// The source queue's redrive policy targets that DLQ with the right count.
	srcURL := mustQueueURL(t, client, topic)
	redrive := mustQueueAttribute(t, client, srcURL, types.QueueAttributeNameRedrivePolicy)
	require.NotEmpty(t, redrive, "source queue should have a redrive policy")

	var policy struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
		MaxReceiveCount     int    `json:"maxReceiveCount"`
	}
	require.NoError(t, json.Unmarshal([]byte(redrive), &policy))
	require.Equal(t, dlqArn, policy.DeadLetterTargetArn)
	require.Equal(t, 3, policy.MaxReceiveCount)
}

// TestDeadLetterQueue_MovesFailedMessages verifies end-to-end that a message
// repeatedly nacked beyond MaxReceiveCount is redriven by SQS to the DLQ, where
// a plain subscriber can consume it.
func TestDeadLetterQueue_MovesFailedMessages(t *testing.T) {
	t.Parallel()

	cfg := newAwsConfig(t)

	sub, err := sqs.NewSubscriber(sqs.SubscriberConfig{
		AWSConfig:             cfg,
		OptFns:                []func(*amazonsqs.Options){GetEndpointResolverSqs()},
		QueueConfigAttributes: sqs.QueueConfigAttributes{VisibilityTimeout: "1"},
		Unmarshaler:           sqs.DefaultMarshalerUnmarshaler{},
		DeadLetterQueue:       &sqs.DeadLetterQueueConfig{MaxReceiveCount: 2},
	}, watermill.NewStdLogger(false, false))
	require.NoError(t, err)
	defer func() { _ = sub.Close() }()

	topic := fmt.Sprintf("dlq-redrive-%s", watermill.NewUUID())
	require.NoError(t, sub.SubscribeInitialize(topic))

	pub, err := sqs.NewPublisher(sqs.PublisherConfig{
		AWSConfig: cfg,
		OptFns:    []func(*amazonsqs.Options){GetEndpointResolverSqs()},
		Marshaler: sqs.DefaultMarshalerUnmarshaler{},
	}, watermill.NewStdLogger(false, false))
	require.NoError(t, err)
	defer func() { _ = pub.Close() }()

	require.NoError(t, pub.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte("boom"))))

	// Consume the source queue and always nack, exhausting the receive budget.
	consumeCtx, consumeCancel := context.WithCancel(context.Background())
	defer consumeCancel()
	out, err := sub.Subscribe(consumeCtx, topic)
	require.NoError(t, err)

	var nacks atomic.Int32
	go func() {
		for msg := range out {
			nacks.Add(1)
			msg.Nack()
		}
	}()

	// A plain subscriber on the DLQ must eventually receive the failed message.
	dlqSub, err := sqs.NewSubscriber(sqs.SubscriberConfig{
		AWSConfig:   cfg,
		OptFns:      []func(*amazonsqs.Options){GetEndpointResolverSqs()},
		Unmarshaler: sqs.DefaultMarshalerUnmarshaler{},
	}, watermill.NewStdLogger(false, false))
	require.NoError(t, err)
	defer func() { _ = dlqSub.Close() }()

	dlqCtx, dlqCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dlqCancel()
	dlqOut, err := dlqSub.Subscribe(dlqCtx, topic+"-dlq")
	require.NoError(t, err)

	select {
	case msg, ok := <-dlqOut:
		require.True(t, ok, "DLQ channel closed before delivering the failed message")
		msg.Ack()
	case <-dlqCtx.Done():
		t.Fatalf("message was not moved to the DLQ (nacks so far: %d)", nacks.Load())
	}
}

func mustQueueURL(t *testing.T, client *amazonsqs.Client, name string) string {
	t.Helper()
	out, err := client.GetQueueUrl(context.Background(), &amazonsqs.GetQueueUrlInput{
		QueueName: aws.String(name),
	})
	require.NoError(t, err, "queue %q should exist", name)
	return *out.QueueUrl
}

func mustQueueAttribute(t *testing.T, client *amazonsqs.Client, queueURL string, attr types.QueueAttributeName) string {
	t.Helper()
	out, err := client.GetQueueAttributes(context.Background(), &amazonsqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{attr},
	})
	require.NoError(t, err)
	return out.Attributes[string(attr)]
}
