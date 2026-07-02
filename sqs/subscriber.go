package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
)

type Subscriber struct {
	config SubscriberConfig
	logger watermill.LoggerAdapter
	sqs    *sqs.Client

	closing       chan struct{}
	subscribersWg sync.WaitGroup

	closed     bool
	closedLock sync.Mutex
}

func NewSubscriber(config SubscriberConfig, logger watermill.LoggerAdapter) (*Subscriber, error) {
	config.SetDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}

	if logger == nil {
		logger = watermill.NopLogger{}
	}

	logger = logger.With(watermill.LogFields{
		"subscriber_uuid": watermill.NewShortUUID(),
	})

	return &Subscriber{
		config:  config,
		logger:  logger,
		sqs:     sqs.NewFromConfig(config.AWSConfig, config.OptFns...),
		closing: make(chan struct{}),
	}, nil
}

func (s *Subscriber) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	if s.closed {
		return nil, errors.New("subscriber closed")
	}

	s.logger.With(watermill.LogFields{"topic": topic}).Debug("Getting queue", nil)

	resolveQueueParams := ResolveQueueUrlParams{
		Topic:     topic,
		SqsClient: s.sqs,
		Logger:    s.logger,
	}

	resolvedQueue, err := s.config.QueueUrlResolver.ResolveQueueUrl(ctx, resolveQueueParams)
	if err != nil {
		return nil, err
	}
	// if we already know we are creating the queue - if not we'll create it later
	if resolvedQueue.Exists != nil && !*resolvedQueue.Exists {
		if s.config.DoNotCreateQueueIfNotExists {
			return nil, fmt.Errorf("queue for topic '%s' doesn't exists", topic)
		}

		if err := s.createSourceQueue(ctx, resolvedQueue.QueueName, topic); err != nil {
			return nil, err
		}

		resolvedQueue, err = s.config.QueueUrlResolver.ResolveQueueUrl(ctx, resolveQueueParams)
		if err != nil {
			return nil, err
		}
	}

	receiveInput, err := s.config.GenerateReceiveMessageInput(ctx, *resolvedQueue.QueueURL)
	if err != nil {
		return nil, fmt.Errorf("cannot generate input for topic %s: %w", topic, err)
	}

	s.logger.With(watermill.LogFields{"queue": *resolvedQueue.QueueURL}).Info("Subscribing to queue", nil)

	ctx, cancel := context.WithCancel(ctx)
	output := make(chan *message.Message)

	var workersWg sync.WaitGroup
	for i := 0; i < s.config.ConsumeWorkers; i++ {
		workersWg.Add(1)
		go func() {
			defer workersWg.Done()
			s.receive(ctx, *resolvedQueue.QueueURL, output, receiveInput)
		}()
	}

	s.subscribersWg.Add(1)
	go func() {
		defer s.subscribersWg.Done()
		workersWg.Wait()
		close(output)
		cancel()
	}()

	return output, nil
}

func (s *Subscriber) receive(ctx context.Context, queueURL QueueURL, output chan *message.Message, input *sqs.ReceiveMessageInput) {
	ctx, cancelCtx := context.WithCancel(ctx)
	defer cancelCtx()
	logFields := watermill.LogFields{
		"provider": "aws",
		"queue":    queueURL,
	}

	go func() {
		<-s.closing
		cancelCtx()
	}()

	var sleepTime time.Duration = 0
	for {
		select {
		case <-s.closing:
			s.logger.Debug("Discarding queued message, subscriber closing", logFields)
			return

		case <-ctx.Done():
			s.logger.Debug("Stopping consume, context canceled", logFields)
			return

		case <-time.After(sleepTime):
			// Wait if needed
		}

		result, err := s.sqs.ReceiveMessage(ctx, input)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				sleepTime = NoSleep
				continue
			} else {
				s.logger.Error("Cannot connect receive messages", err, logFields)
				sleepTime = s.config.ReconnectRetrySleep
				continue
			}
		}

		sleepTime = NoSleep
		if result == nil || len(result.Messages) == 0 {
			s.logger.Trace("No messages", logFields)
			continue
		}
		s.consumeMessages(ctx, result.Messages, queueURL, output, logFields)
	}
}

func (s *Subscriber) consumeMessages(
	ctx context.Context,
	messages []types.Message,
	queueURL QueueURL,
	output chan *message.Message,
	logFields watermill.LogFields,
) {
	for _, sqsMsg := range messages {
		processed := s.processMessage(ctx, logFields, sqsMsg, output, queueURL)

		if !processed {
			return
		}
	}
}

func (s *Subscriber) processMessage(
	ctx context.Context,
	logFields watermill.LogFields,
	sqsMsg types.Message,
	output chan *message.Message,
	queueURL QueueURL,
) bool {
	logger := s.logger.With(logFields)
	logger.Trace("processMessage", nil)

	// handlerCtx is the context the handler sees. When graceful draining is
	// enabled we detach it from receiver/close cancellation so an in-flight
	// handler can finish during shutdown instead of being aborted mid-work; its
	// lifetime is then bounded by the drain deadline below (and by any
	// handler-side timeout). With draining disabled it stays tied to the
	// receiver context, preserving the previous cancel-on-close behavior.
	handlerCtx := ctx
	if s.config.CloseTimeout > 0 {
		handlerCtx = context.WithoutCancel(ctx)
	}
	handlerCtx, cancelMsg := context.WithCancel(handlerCtx)
	defer cancelMsg()

	msg, err := s.config.Unmarshaler.Unmarshal(&sqsMsg)
	if err != nil {
		logger.Error("Cannot unmarshal message", err, logFields)
		return false
	}
	msg.SetContext(handlerCtx)

	logger = s.logger.With(logFields).With(watermill.LogFields{
		"message_uuid": msg.UUID,
	})

	select {
	case output <- msg:
	case <-s.closing:
		logger.Debug("Closing, message discarded before send", logFields)
		return false
	case <-ctx.Done():
		logger.Debug("Closing, ctx cancelled before send", logFields)
		return false
	}

	// Wait for the handler to finish. On shutdown (s.closing, or the receiver
	// context being canceled) we begin a bounded drain rather than abandoning
	// the message outright: the handler keeps running for up to CloseTimeout so
	// its work can complete and be acked. Only when that deadline elapses do we
	// cancel the handler and abandon the message for redelivery.
	closing := s.closing
	ctxDone := ctx.Done()
	var (
		drainTimer    *time.Timer
		drainDeadline <-chan time.Time
	)
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	startDrain := func() {
		if drainTimer != nil {
			return
		}
		logger.Debug("Subscriber closing, draining in-flight message", logFields)
		drainTimer = time.NewTimer(s.config.CloseTimeout)
		drainDeadline = drainTimer.C
	}

	for {
		select {
		case <-msg.Acked():
			err := s.deleteMessage(handlerCtx, queueURL, sqsMsg.ReceiptHandle, logFields)
			if errors.Is(err, context.Canceled) {
				return false
			}
			if err != nil {
				logger.Error("Failed to delete message", err, logFields)
				return false
			}
			return true
		case <-msg.Nacked():
			// Do not delete message, it will be redelivered
			logger.Debug("Nacking message", logFields)
			return false // we don't want to process next messages to preserve order for FIFO
		case <-closing:
			closing = nil // a closed channel is always ready; react once
			if s.config.CloseTimeout <= 0 {
				logger.Debug("Closing, message discarded before ack", logFields)
				return false
			}
			startDrain()
		case <-ctxDone:
			ctxDone = nil
			if s.config.CloseTimeout <= 0 {
				logger.Debug("Closing, ctx cancelled before ack", logFields)
				return false
			}
			startDrain()
		case <-drainDeadline:
			cancelMsg()
			logger.Info("Drain deadline exceeded before ack, abandoning in-flight message", logFields)
			return false
		}
	}
}

func (s *Subscriber) deleteMessage(ctx context.Context, queueURL QueueURL, receiptHandle *string, logFields watermill.LogFields) error {
	// With draining enabled the ack (and this delete) can happen after shutdown
	// began. The caller passes a cancellation-detached context so the delete is
	// not aborted by close; bound it here so a slow delete can't hang shutdown.
	if s.config.CloseTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.config.CloseTimeout)
		defer cancel()
	}

	input, err := s.config.GenerateDeleteMessageInput(ctx, queueURL, receiptHandle)
	if err != nil {
		return fmt.Errorf("cannot generate input for delete message: %w", err)
	}

	// With draining disabled, ctx may be canceled when the subscriber is closing.
	//
	// it may lead to re-delivery when message is processed and in the meantime
	// subscriber is closed - but we don't know if context cancellation didn't cancel
	// some SQL transactions or whatever - so someone may lose data
	//
	// in other words, we prefer re-delivery (as at least once delivery is a thing anyway)
	_, err = s.sqs.DeleteMessage(ctx, input)
	if err != nil {
		var oe *smithy.GenericAPIError
		if errors.As(err, &oe) {
			// todo(roblaszczak): it would be nice to replace it with a specific error type
			// but I wasn't able to reproduce it
			if oe.Message == "The specified queue does not contain the message specified." {
				s.logger.Debug("Message was already deleted or is not in queue", logFields)
				return nil
			}
		}
		return fmt.Errorf("cannot ack (delete) message: %w", err)
	}

	return nil
}

func (s *Subscriber) Close() error {
	s.closedLock.Lock()
	defer s.closedLock.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	close(s.closing)
	s.subscribersWg.Wait()

	return nil
}

func (s *Subscriber) SubscribeInitialize(topic string) error {
	return s.SubscribeInitializeWithContext(context.Background(), topic)
}

func (s *Subscriber) SubscribeInitializeWithContext(ctx context.Context, topic string) error {
	logger := s.logger.With(watermill.LogFields{
		"topic": topic,
	})
	logger.Debug("Initializing SQS subscription", nil)

	resolvedQueue, err := s.config.QueueUrlResolver.ResolveQueueUrl(ctx, ResolveQueueUrlParams{
		Topic:     topic,
		SqsClient: s.sqs,
		Logger:    s.logger,
	})

	logger.Debug("Topic resolving done", watermill.LogFields{
		"resolved_queue": resolvedQueue,
		"err":            err,
	})
	if err != nil {
		return err
	}
	if resolvedQueue.Exists != nil && *resolvedQueue.Exists {
		return nil
	}

	if s.config.DoNotCreateQueueIfNotExists {
		return fmt.Errorf("queue for topic '%s' doesn't exists", topic)
	}

	if err := s.createSourceQueue(ctx, resolvedQueue.QueueName, topic); err != nil {
		return err
	}

	return nil
}

// createSourceQueue creates the queue for a topic. When
// SubscriberConfig.DeadLetterQueue is set it first provisions the dead-letter
// queue and injects a redrive policy into the source queue's attributes so
// failed messages are moved there.
func (s *Subscriber) createSourceQueue(ctx context.Context, queueName QueueName, topic string) error {
	input, err := s.config.GenerateCreateQueueInput(ctx, queueName, s.config.QueueConfigAttributes)
	if err != nil {
		return fmt.Errorf("cannot generate input for queue %s: %w", topic, err)
	}

	if s.config.DeadLetterQueue != nil {
		dlqArn, dlqErr := s.ensureDeadLetterQueue(ctx, queueName)
		if dlqErr != nil {
			return fmt.Errorf("cannot provision dead-letter queue for %s: %w", topic, dlqErr)
		}
		redrivePolicy, marshalErr := json.Marshal(map[string]any{
			"deadLetterTargetArn": string(dlqArn),
			"maxReceiveCount":     s.config.DeadLetterQueue.MaxReceiveCount,
		})
		if marshalErr != nil {
			return fmt.Errorf("cannot marshal redrive policy for %s: %w", topic, marshalErr)
		}
		if input.Attributes == nil {
			input.Attributes = map[string]string{}
		}
		input.Attributes[string(types.QueueAttributeNameRedrivePolicy)] = string(redrivePolicy)
	}

	s.logger.Debug("Creating queue", watermill.LogFields{"queue_name": *input.QueueName})

	if _, err := createQueue(ctx, s.sqs, input); err != nil {
		return fmt.Errorf("cannot create queue %s: %w", topic, err)
	}
	return nil
}

// ensureDeadLetterQueue provisions (idempotently) the dead-letter queue for a
// source queue and returns its ARN: it creates the DLQ when missing and
// refreshes its attributes and tags when it already exists.
func (s *Subscriber) ensureDeadLetterQueue(ctx context.Context, sourceQueueName QueueName) (QueueArn, error) {
	dlq := s.config.DeadLetterQueue
	dlqName := dlq.GenerateName(sourceQueueName)

	input, err := dlq.GenerateCreateQueueInput(ctx, dlqName, dlq.QueueConfigAttributes)
	if err != nil {
		return "", fmt.Errorf("cannot generate create input for dead-letter queue %s: %w", dlqName, err)
	}

	logFields := watermill.LogFields{"dlq_name": dlqName}

	var dlqURL QueueURL
	getOutput, getErr := s.sqs.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(string(dlqName))})
	var notExist *types.QueueDoesNotExist
	switch {
	case getErr == nil && getOutput.QueueUrl != nil:
		dlqURL = QueueURL(*getOutput.QueueUrl)
		s.logger.Debug("Dead-letter queue already exists, refreshing", logFields)
		if len(input.Attributes) > 0 {
			if _, aerr := s.sqs.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
				QueueUrl:   aws.String(string(dlqURL)),
				Attributes: input.Attributes,
			}); aerr != nil {
				s.logger.Error("Failed to update dead-letter queue attributes", aerr, logFields)
			}
		}
		if len(input.Tags) > 0 {
			if _, terr := s.sqs.TagQueue(ctx, &sqs.TagQueueInput{
				QueueUrl: aws.String(string(dlqURL)),
				Tags:     input.Tags,
			}); terr != nil {
				return "", fmt.Errorf("cannot tag dead-letter queue %s: %w", dlqName, terr)
			}
		}
	case errors.As(getErr, &notExist):
		s.logger.Info("Creating dead-letter queue", logFields)
		created, cerr := createQueue(ctx, s.sqs, input)
		if cerr != nil {
			return "", fmt.Errorf("cannot create dead-letter queue %s: %w", dlqName, cerr)
		}
		if created != nil {
			dlqURL = *created
		} else {
			// createQueue swallows QueueNameExists (created concurrently); resolve it.
			resolved, rerr := getQueueUrl(ctx, s.sqs, string(dlqName), &sqs.GetQueueUrlInput{QueueName: aws.String(string(dlqName))})
			if rerr != nil {
				return "", fmt.Errorf("cannot resolve dead-letter queue %s after create: %w", dlqName, rerr)
			}
			dlqURL = *resolved
		}
	default:
		return "", fmt.Errorf("cannot resolve dead-letter queue %s: %w", dlqName, getErr)
	}

	arn, err := getARNUrl(ctx, s.sqs, &dlqURL)
	if err != nil {
		return "", fmt.Errorf("cannot get ARN for dead-letter queue %s: %w", dlqName, err)
	}
	return *arn, nil
}

func (s *Subscriber) GetQueueUrl(ctx context.Context, topic string) (*QueueURL, error) {
	resolvedQueue, err := s.config.QueueUrlResolver.ResolveQueueUrl(ctx, ResolveQueueUrlParams{
		Topic:     topic,
		SqsClient: s.sqs,
		Logger:    s.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot generate input for queue %s: %w", topic, err)
	}
	if resolvedQueue.Exists != nil && !*resolvedQueue.Exists {
		return nil, fmt.Errorf("queue for topic '%s' doesn't exist", topic)
	}

	return resolvedQueue.QueueURL, nil
}

func (s *Subscriber) GetQueueArn(ctx context.Context, url *QueueURL) (*QueueArn, error) {
	return getARNUrl(ctx, s.sqs, url)
}

const NoSleep time.Duration = -1
