package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type SubscriberConfig struct {
	// AWSConfig is the AWS configuration.
	AWSConfig aws.Config

	// OptFns are options for the SQS client.
	OptFns []func(*sqs.Options)

	// DoNotCreateQueueIfNotExists disables creating the queue if it does not exist.
	DoNotCreateQueueIfNotExists bool

	// QueueUrlResolver is a function that resolves the queue name to the queue URL.
	QueueUrlResolver QueueUrlResolver

	// ReconnectRetrySleep is the time to sleep between reconnect attempts.
	ReconnectRetrySleep time.Duration

	// QueueConfigAttributes is a struct that holds the attributes of an SQS queue.
	QueueConfigAttributes QueueConfigAttributes

	// GenerateCreateQueueInput generates *sqs.CreateQueueInput for AWS SDK.
	GenerateCreateQueueInput GenerateCreateQueueInputFunc

	// GenerateReceiveMessageInput generates *sqs.ReceiveMessageInput for AWS SDK.
	GenerateReceiveMessageInput GenerateReceiveMessageInputFunc

	// GenerateDeleteMessageInput generates *sqs.DeleteMessageInput for AWS SDK.
	GenerateDeleteMessageInput GenerateDeleteMessageInputFunc

	// ConsumeWorkers is the number of goroutines that will concurrently receive and process messages
	// from the SQS queue per Subscribe call. Each worker independently polls SQS and processes messages.
	// Defaults to 1.
	ConsumeWorkers int

	// CloseTimeout, when > 0, enables graceful draining on shutdown: once the
	// subscriber starts closing, an in-flight message is not abandoned
	// immediately. Its handler keeps a live (cancellation-detached) context and
	// is given up to CloseTimeout to finish and ack, in which case the message
	// is deleted normally instead of being redelivered. Only if the handler is
	// still running when CloseTimeout elapses is its context canceled and the
	// message abandoned for SQS to redeliver.
	//
	// When 0 (default), the previous behavior applies: in-flight handlers are
	// canceled the moment the subscriber closes and their messages are left for
	// redelivery.
	CloseTimeout time.Duration

	// DeadLetterQueue, when non-nil, makes the subscriber provision a companion
	// dead-letter queue for each source queue it creates and attach a redrive
	// policy, so a message that fails delivery MaxReceiveCount times is moved to
	// the DLQ instead of being retried indefinitely. Nil (default) disables it.
	DeadLetterQueue *DeadLetterQueueConfig

	// DeleteBatch, when non-nil, batches acks: instead of one DeleteMessage
	// call per acked message, receipt handles are buffered per queue and
	// deleted with DeleteMessageBatch (up to 10 per call), cutting SQS request
	// count — and cost — by up to 10x on high-volume queues. Acking also stops
	// blocking the consume worker on the delete round-trip.
	//
	// Semantics change vs the default: the delete becomes asynchronous, so a
	// failed delete can no longer stop the worker from consuming the rest of
	// its received batch — the failure is logged and the message is simply
	// redelivered after the visibility timeout (the same at-least-once outcome
	// as a failed synchronous delete). Do not enable on FIFO queues if you
	// rely on that stop-on-failure ordering. The GenerateDeleteMessageInput
	// hook is not applied on the batch path.
	//
	// Nil (default) preserves the one-DeleteMessage-per-ack behavior.
	DeleteBatch *DeleteBatchConfig

	Unmarshaler Unmarshaler
}

// DeleteBatchConfig configures batched message deletion. See
// SubscriberConfig.DeleteBatch.
type DeleteBatchConfig struct {
	// MaxSize is the number of buffered receipt handles that triggers an
	// immediate flush. SQS caps batch entries at 10; values outside [1, 10]
	// are clamped into that range. Defaults to 10.
	MaxSize int

	// Linger is the longest a buffered receipt handle waits before being
	// flushed regardless of batch size (worst case ~2x this, due to the
	// ticker-based flusher). Defaults to 500ms. Keep it far below the queue's
	// VisibilityTimeout: an acked-but-not-yet-deleted message whose visibility
	// expires is redelivered.
	Linger time.Duration
}

func (c *DeleteBatchConfig) setDefaults() {
	if c.MaxSize <= 0 || c.MaxSize > sqsMaxBatchSize {
		c.MaxSize = sqsMaxBatchSize
	}
	if c.Linger <= 0 {
		c.Linger = 500 * time.Millisecond
	}
}

// sqsMaxBatchSize is the SQS API limit on entries per DeleteMessageBatch call.
const sqsMaxBatchSize = 10

func (c *SubscriberConfig) SetDefaults() {
	if c.Unmarshaler == nil {
		c.Unmarshaler = DefaultMarshalerUnmarshaler{}
	}

	if c.ReconnectRetrySleep == 0 {
		c.ReconnectRetrySleep = time.Second
	}

	if c.GenerateCreateQueueInput == nil {
		c.GenerateCreateQueueInput = GenerateCreateQueueInputDefault
	}

	if c.GenerateReceiveMessageInput == nil {
		c.GenerateReceiveMessageInput = GenerateReceiveMessageInputDefault
	}

	if c.GenerateDeleteMessageInput == nil {
		c.GenerateDeleteMessageInput = GenerateDeleteMessageInputDefault
	}

	if c.QueueUrlResolver == nil {
		c.QueueUrlResolver = NewGetQueueUrlByNameUrlResolver(GetQueueUrlByNameUrlResolverConfig{})
	}

	if c.ConsumeWorkers <= 0 {
		c.ConsumeWorkers = 1
	}

	if c.DeadLetterQueue != nil {
		c.DeadLetterQueue.setDefaults()
	}

	if c.DeleteBatch != nil {
		c.DeleteBatch.setDefaults()
	}
}

func (c SubscriberConfig) Validate() error {
	var err error

	if c.AWSConfig.Credentials == nil {
		err = errors.Join(err, errors.New("missing Config.Credentials"))

	}
	if c.Unmarshaler == nil {
		err = errors.Join(err, errors.New("missing Config.Marshaler"))
	}
	if c.QueueUrlResolver == nil {
		err = errors.Join(err, fmt.Errorf("sqs.SubscriberConfig.QueueUrlResolver is nil"))
	}
	if c.DeadLetterQueue != nil {
		err = errors.Join(err, c.DeadLetterQueue.validate())
	}

	return err
}

// DeadLetterQueueConfig configures automatic dead-letter queue provisioning for
// a subscriber. See SubscriberConfig.DeadLetterQueue.
type DeadLetterQueueConfig struct {
	// MaxReceiveCount is the number of times SQS delivers a message before
	// moving it to the dead-letter queue. Must be >= 1.
	MaxReceiveCount int

	// GenerateName derives the dead-letter queue's name from the source queue's
	// name. Defaults to DeadLetterQueueNameDefault ("<source>-dlq", FIFO-aware,
	// truncated to the 80-character SQS limit).
	GenerateName func(sourceQueueName QueueName) QueueName

	// GenerateCreateQueueInput builds the CreateQueueInput for the dead-letter
	// queue itself. Defaults to GenerateCreateQueueInputDefault; override it to
	// set the DLQ's retention period, tags, etc.
	GenerateCreateQueueInput GenerateCreateQueueInputFunc

	// QueueConfigAttributes is passed to GenerateCreateQueueInput when creating
	// the dead-letter queue.
	QueueConfigAttributes QueueConfigAttributes
}

func (c *DeadLetterQueueConfig) setDefaults() {
	if c.GenerateName == nil {
		c.GenerateName = DeadLetterQueueNameDefault
	}
	if c.GenerateCreateQueueInput == nil {
		c.GenerateCreateQueueInput = GenerateCreateQueueInputDefault
	}
}

func (c *DeadLetterQueueConfig) validate() error {
	if c.MaxReceiveCount < 1 {
		return fmt.Errorf("sqs.DeadLetterQueueConfig.MaxReceiveCount must be >= 1, got %d", c.MaxReceiveCount)
	}
	return nil
}

// maxQueueNameLength is the SQS hard limit on queue name length.
const maxQueueNameLength = 80

// DeadLetterQueueNameDefault derives a dead-letter queue name from a source
// queue name by appending "-dlq", keeping a trailing ".fifo" in place for FIFO
// queues, and truncating to the 80-character SQS limit.
func DeadLetterQueueNameDefault(sourceQueueName QueueName) QueueName {
	const suffix = "-dlq"
	const fifoSuffix = ".fifo"

	name := string(sourceQueueName)
	if strings.HasSuffix(name, fifoSuffix) {
		base := truncateQueueName(strings.TrimSuffix(name, fifoSuffix)+suffix, maxQueueNameLength-len(fifoSuffix))
		return QueueName(base + fifoSuffix)
	}
	return QueueName(truncateQueueName(name+suffix, maxQueueNameLength))
}

func truncateQueueName(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}
	return name[:maxLen]
}

type PublisherConfig struct {
	// AWSConfig is the AWS configuration.
	AWSConfig aws.Config

	// OptFns are options for the SQS client.
	OptFns []func(*sqs.Options)

	// QueueConfigAttributes is a struct that holds the attributes of an SQS queue.
	CreateQueueConfig QueueConfigAttributes

	// DoNotCreateQueueIfNotExists disables creating the queue if it does not exist.
	DoNotCreateQueueIfNotExists bool

	// QueueUrlResolver is a function that resolves queue URL.
	QueueUrlResolver QueueUrlResolver

	// GenerateSendMessageInput generates *sqs.SendMessageInput for AWS SDK.
	GenerateSendMessageInput GenerateSendMessageInputFunc

	// GenerateCreateQueueInput generates *sqs.CreateQueueInput for AWS SDK.
	GenerateCreateQueueInput GenerateCreateQueueInputFunc

	Marshaler Marshaler
}

func (c *PublisherConfig) setDefaults() {
	if c.Marshaler == nil {
		c.Marshaler = DefaultMarshalerUnmarshaler{}
	}

	if c.GenerateSendMessageInput == nil {
		c.GenerateSendMessageInput = GenerateSendMessageInputDefault
	}

	if c.GenerateCreateQueueInput == nil {
		c.GenerateCreateQueueInput = GenerateCreateQueueInputDefault
	}

	if c.QueueUrlResolver == nil {
		c.QueueUrlResolver = NewGetQueueUrlByNameUrlResolver(GetQueueUrlByNameUrlResolverConfig{})
	}
}

func (c *PublisherConfig) Validate() error {
	var err error

	if c.QueueUrlResolver == nil {
		err = errors.Join(err, fmt.Errorf("sqs.SubscriberConfig.QueueUrlResolver is nil"))
	}

	return err
}

type GenerateCreateQueueInputFunc func(ctx context.Context, queueName QueueName, attrs QueueConfigAttributes) (*sqs.CreateQueueInput, error)

func GenerateCreateQueueInputDefault(ctx context.Context, queueName QueueName, attrs QueueConfigAttributes) (*sqs.CreateQueueInput, error) {
	attrsMap, err := attrs.Attributes()
	if err != nil {
		return nil, fmt.Errorf("cannot generate attributes for queue %s: %w", queueName, err)
	}

	return &sqs.CreateQueueInput{
		QueueName:  aws.String(string(queueName)),
		Attributes: attrsMap,
	}, nil
}

type GenerateReceiveMessageInputFunc func(ctx context.Context, queueURL QueueURL) (*sqs.ReceiveMessageInput, error)

func GenerateReceiveMessageInputDefault(ctx context.Context, queueURL QueueURL) (*sqs.ReceiveMessageInput, error) {
	return &sqs.ReceiveMessageInput{
		QueueUrl:              aws.String(string(queueURL)),
		MessageAttributeNames: []string{"All"},
		WaitTimeSeconds:       20, // 20 is max at the moment
		MaxNumberOfMessages:   1,  // Currently default value.
	}, nil
}

type GenerateDeleteMessageInputFunc func(ctx context.Context, queueURL QueueURL, receiptHandle *string) (*sqs.DeleteMessageInput, error)

func GenerateDeleteMessageInputDefault(ctx context.Context, queueURL QueueURL, receiptHandle *string) (*sqs.DeleteMessageInput, error) {
	return &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(string(queueURL)),
		ReceiptHandle: receiptHandle,
	}, nil
}

type GenerateSendMessageInputFunc func(ctx context.Context, queueURL QueueURL, msg *types.Message) (*sqs.SendMessageInput, error)

func GenerateSendMessageInputDefault(ctx context.Context, queueURL QueueURL, msg *types.Message) (*sqs.SendMessageInput, error) {
	urlStr := string(queueURL)

	return &sqs.SendMessageInput{
		QueueUrl:          &urlStr,
		MessageAttributes: msg.MessageAttributes,
		MessageBody:       msg.Body,
	}, nil
}

// QueueConfigAttributes is a struct that holds the attributes of an SQS queue.
// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_SetQueueAttributes.html
type QueueConfigAttributes struct {
	// DelaySeconds – The length of time, in seconds, for which the delivery of all messages in the queue is delayed.
	// Valid values: An integer from 0 to 900 (15 minutes). Default: 0.
	DelaySeconds string `json:"DelaySeconds,omitempty"`

	// MaximumMessageSize – The limit of how many bytes a message can contain before Amazon SQS rejects it.
	// Valid values: An integer from 1,024 bytes (1 KiB) up to 262,144 bytes (256 KiB). Default: 262,144 (256 KiB).
	MaximumMessageSize string `json:"MaximumMessageSize,omitempty"`

	// MessageRetentionPeriod – The length of time, in seconds, for which Amazon SQS retains a message.
	// Valid values: An integer representing seconds, from 60 (1 minute) to 1,209,600 (14 days).
	// Default: 345,600 (4 days).
	//
	// When you change a queue's attributes, the change can take up to 60 seconds for most of the attributes to
	// propagate throughout the Amazon SQS system. Changes made to the MessageRetentionPeriod attribute can take up to
	// 15 minutes and will impact existing messages in the queue potentially causing them to be expired and deleted if
	// the MessageRetentionPeriod is reduced below the age of existing messages.
	MessageRetentionPeriod string `json:"MessageRetentionPeriod,omitempty"`

	// Policy – The queue's policy. A valid AWS policy. For more information about policy structure, see
	// [Overview of AWS IAM Policies](https://docs.aws.amazon.com/IAM/latest/UserGuide/access_policies.html) in the
	// AWS Identity and Access Management User Guide.
	Policy string `json:"Policy,omitempty"`

	// ReceiveMessageWaitTimeSeconds – The length of time, in seconds, for which a ReceiveMessage action waits for a
	// message to arrive.
	// Valid values: An integer from 0 to 20 (seconds). Default: 0.
	ReceiveMessageWaitTimeSeconds string `json:"ReceiveMessageWaitTimeSeconds,omitempty"`

	// RedrivePolicy – The string that includes the parameters for the dead-letter queue functionality of the source
	// queue as a JSON object. The parameters are as follows:
	RedrivePolicy string `json:"RedrivePolicy,omitempty"`

	// DeadLetterTargetArn – The Amazon Resource Name (ARN) of the dead-letter queue to which Amazon SQS moves
	// messages after the value of maxReceiveCount is exceeded.
	DeadLetterTargetArn string `json:"deadLetterTargetArn,omitempty"`

	// MaxReceiveCount – The number of times a message is delivered to the source queue before being moved to the
	// dead-letter queue. Default: 10. When the ReceiveCount for a message exceeds the maxReceiveCount for a queue,
	// Amazon SQS moves the message to the dead-letter-queue.
	MaxReceiveCount string `json:"maxReceiveCount,omitempty"`

	// VisibilityTimeout – The visibility timeout for the queue, in seconds.
	// Valid values: An integer from 0 to 43,200 (12 hours). Default: 30.
	//
	// For more information about the visibility timeout, see
	// [Visibility Timeout](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html) in the Amazon SQS Developer Guide.
	VisibilityTimeout string `json:"VisibilityTimeout,omitempty"`

	// KmsMasterKeyId – The ID of an AWS managed customer master key (CMK) for Amazon SQS or a custom CMK.
	// For more information, see [Key Terms](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-server-side-encryption.html#sqs-sse-key-terms).
	//
	// While the alias of the AWS-managed CMK for Amazon SQS is always
	// alias/aws/sqs, the alias of a custom CMK can, for example, be alias/MyAlias.
	// For more examples, see [KeyId](https://docs.aws.amazon.com/kms/latest/APIReference/API_DescribeKey.html#API_DescribeKey_RequestParameters)
	// in the AWS Key Management Service API Reference.
	KmsMasterKeyId string `json:"KmsMasterKeyId,omitempty"`

	// KmsDataKeyReusePeriodSeconds – The length of time, in seconds, for which Amazon SQS can reuse a [data key](https://docs.aws.amazon.com/kms/latest/developerguide/concepts.html#data-keys)
	// to encrypt or decrypt messages before calling AWS KMS again.
	// An integer representing seconds, between 60 seconds (1 minute) and 86,400 seconds (24 hours).
	// Default: 300 (5 minutes).
	//
	// A shorter time period provides better security but results in more calls to KMS which might incur charges after Free Tier.
	// For more information, see How Does the [Data Key Reuse Period Work?](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-server-side-encryption.html#sqs-how-does-the-data-key-reuse-period-work).
	KmsDataKeyReusePeriodSeconds string `json:"KmsDataKeyReusePeriodSeconds,omitempty"`

	// SqsManagedSseEnabled – Enables server-side queue encryption using SQS owned encryption keys.
	// Only one server-side encryption option is supported per queue (for example, [SSE-KMS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-configure-sse-existing-queue.html) or [SSE-SQS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-configure-sqs-sse-queue.html).
	SqsManagedSseEnabled string `json:"SqsManagedSseEnabled,omitempty"`

	// FifoQueue - Designates a queue as FIFO. Valid values: true, false. Default: false.
	FifoQueue QueueConfigAttributesBool `json:"FifoQueue,omitempty"`

	// ContentBasedDeduplication – Enables content-based deduplication. For more information, see [Exactly-once processing](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/FIFO-queues-exactly-once-processing.html)
	// in the Amazon SQS Developer Guide. Note the following:
	//
	// Every message must have a unique MessageDeduplicationId.
	// You may provide a MessageDeduplicationId explicitly.

	// If you aren't able to provide a MessageDeduplicationId and you enable ContentBasedDeduplication for your queue,
	// Amazon SQS uses a SHA-256 hash to generate the MessageDeduplicationId using the body of the message
	// (but not the attributes of the message).

	// If you don't provide a MessageDeduplicationId and the queue doesn't have ContentBasedDeduplication set, the
	// action fails with an error.
	// If the queue has ContentBasedDeduplication set, your MessageDeduplicationId overrides the generated one.
	// When ContentBasedDeduplication is in effect, messages with identical content sent within the deduplication
	// interval are treated as duplicates and only one copy of the message is delivered.
	//
	// If you send one message with ContentBasedDeduplication enabled and then another message with a
	// MessageDeduplicationId that is the same as the one generated for the first MessageDeduplicationId, the two
	// messages are treated as duplicates and only one copy of the message is delivered.
	ContentBasedDeduplication QueueConfigAttributesBool `json:"ContentBasedDeduplication,omitempty"`

	// DeduplicationScope – Specifies whether message deduplication occurs at the message group or queue level.
	// Valid values are messageGroup and queue.
	//
	// Apply only to [high throughput for FIFO queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/high-throughput-fifo.html).
	DeduplicationScope string `json:"DeduplicationScope,omitempty"`

	// FifoThroughputLimit – Specifies whether the FIFO queue throughput quota applies to the entire queue or per message group.
	// Valid values are perQueue and perMessageGroupId.
	// The perMessageGroupId value is allowed only when the value for DeduplicationScope is messageGroup.
	// Apply only to [high throughput for FIFO queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/high-throughput-fifo.html).
	FifoThroughputLimit string `json:"FifoThroughputLimit,omitempty"`

	// CustomAttributes is a map of custom attributes that are not mapped to the struct fields.
	CustomAttributes map[string]string `json:"-"`
}

// QueueConfigAttributesBool is a custom type for bool values in QueueConfigAttributes
// that supports marshaling to string.
type QueueConfigAttributesBool bool

func (q QueueConfigAttributesBool) MarshalText() ([]byte, error) {
	if q {
		return []byte("true"), nil
	}
	return []byte("false"), nil
}

func (q QueueConfigAttributes) Attributes() (map[string]string, error) {
	b, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal queue attributes (json.Marshal): %w", err)
	}

	var m map[string]string
	err = json.Unmarshal(b, &m)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal queue attributes (json.Unmarshal): %w", err)
	}

	if q.CustomAttributes != nil {
		for k, v := range q.CustomAttributes {
			m[k] = v
		}
	}

	return m, nil
}
