package googlecloud

import (
	"context"
	"sync"

	"google.golang.org/api/option"

	"cloud.google.com/go/pubsub"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/pkg/errors"
)

var (
	ErrSubscriberClosed         = errors.New("subscriber is closed")
	ErrSubscriptionDoesNotExist = errors.New("subscription does not exist")
)

type Subscriber struct {
	ctx     context.Context
	closing chan struct{}
	closed  bool

	allSubscriptionsWaitGroup sync.WaitGroup
	activeSubscriptions       map[string]*pubsub.Subscription
	activeSubscriptionsLock   sync.RWMutex

	client *pubsub.Client
	config SubscriberConfig

	logger watermill.LoggerAdapter
}

type SubscriberConfig struct {
	SubscriptionName SubscriptionNameFn
	ProjectID        string

	DoNotCreateSubscriptionIfMissing bool
	DoNotCreateTopicIfMissing        bool

	ReceiveSettings    pubsub.ReceiveSettings
	SubscriptionConfig pubsub.SubscriptionConfig
	ClientOptions      []option.ClientOption
	Unmarshaler        Unmarshaler
}

type SubscriptionNameFn func(topic string) string

func DefaultSubscriptionName(topic string) string {
	return topic
}

func DefaultSubscriptionNameWithSuffix(suffix string) SubscriptionNameFn {
	return func(topic string) string {
		return topic + suffix
	}
}

func (c *SubscriberConfig) setDefaults() {
	if c.SubscriptionName == nil {
		c.SubscriptionName = DefaultSubscriptionName
	}
	if c.Unmarshaler == nil {
		c.Unmarshaler = DefaultMarshalerUnmarshaler{}
	}
}

func NewSubscriber(
	ctx context.Context,
	config SubscriberConfig,
	logger watermill.LoggerAdapter,
) (*Subscriber, error) {
	config.setDefaults()

	client, err := pubsub.NewClient(ctx, config.ProjectID, config.ClientOptions...)
	if err != nil {
		return nil, err
	}

	return &Subscriber{
		ctx:     ctx,
		closing: make(chan struct{}, 1),
		closed:  false,

		allSubscriptionsWaitGroup: sync.WaitGroup{},
		activeSubscriptions:       map[string]*pubsub.Subscription{},
		activeSubscriptionsLock:   sync.RWMutex{},

		client: client,
		config: config,

		logger: logger,
	}, nil
}

func (s *Subscriber) Subscribe(topic string) (chan *message.Message, error) {
	if s.closed {
		return nil, ErrSubscriberClosed
	}

	ctx, cancel := context.WithCancel(s.ctx)
	subscriptionName := s.config.SubscriptionName(topic)

	logFields := watermill.LogFields{
		"provider":          ProviderName,
		"topic":             topic,
		"subscription_name": subscriptionName,
	}
	s.logger.Info("Subscribing to Google Cloud PubSub topic", logFields)

	output := make(chan *message.Message, 0)

	sub, err := s.subscription(ctx, subscriptionName, topic)
	if err != nil {
		s.logger.Error("Could not obtain subscription", err, logFields)
		return nil, err
	}

	receiveFinished := make(chan struct{})
	s.allSubscriptionsWaitGroup.Add(1)
	go func() {
		err := s.receive(ctx, sub, logFields, output)
		if err != nil {
			s.logger.Error("Receiving messages failed", err, logFields)
		}
		close(receiveFinished)
	}()

	go func() {
		<-s.closing
		s.logger.Debug("Closing message consumer", logFields)
		cancel()

		<-receiveFinished
		close(output)
		s.allSubscriptionsWaitGroup.Done()
	}()

	return output, nil
}

func (s *Subscriber) Close() error {
	if s.closed {
		return nil
	}

	s.closed = true
	close(s.closing)
	s.allSubscriptionsWaitGroup.Wait()

	err := s.client.Close()
	if err != nil {
		return err
	}

	s.logger.Debug("Google Cloud PubSub subscriber closed", nil)
	return nil
}

func (s *Subscriber) receive(
	ctx context.Context,
	sub *pubsub.Subscription,
	logFields watermill.LogFields,
	output chan *message.Message,
) error {
	err := sub.Receive(ctx, func(ctx context.Context, pubsubMsg *pubsub.Message) {
		msg, err := s.config.Unmarshaler.Unmarshal(pubsubMsg)
		if err != nil {
			s.logger.Error("Could not unmarshal Google Cloud PubSub message", err, logFields)
			pubsubMsg.Nack()
			return
		}

		select {
		case <-s.closing:
			s.logger.Info(
				"Message not consumed, subscriber is closing",
				logFields,
			)
			pubsubMsg.Nack()
			return
		case output <- msg:
			// message consumed, wait for ack (or nack)
		}

		select {
		case <-s.closing:
			pubsubMsg.Nack()
		case <-msg.Acked():
			pubsubMsg.Ack()
		case <-msg.Nacked():
			pubsubMsg.Nack()
		}
	})

	if err != nil && !s.closed {
		s.logger.Error("Receive failed", err, logFields)
		return err
	}

	return nil
}

// subscription obtains a subscription object.
// If subscription doesn't exist on PubSub, create it, unless config variable DoNotCreateSubscriptionWhenMissing is set.
func (s *Subscriber) subscription(ctx context.Context, subscriptionName, topicName string) (sub *pubsub.Subscription, err error) {
	s.activeSubscriptionsLock.RLock()
	sub, ok := s.activeSubscriptions[subscriptionName]
	s.activeSubscriptionsLock.RUnlock()
	if ok {
		return sub, nil
	}

	s.activeSubscriptionsLock.Lock()
	defer s.activeSubscriptionsLock.Unlock()
	defer func() {
		if err == nil {
			s.activeSubscriptions[subscriptionName] = sub
		}
	}()

	sub = s.client.Subscription(subscriptionName)
	exists, err := sub.Exists(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "could not check if subscription %s exists", subscriptionName)
	}

	if exists {
		return sub, nil
	}

	if s.config.DoNotCreateSubscriptionIfMissing {
		return nil, errors.Wrap(ErrSubscriptionDoesNotExist, subscriptionName)
	}

	t := s.client.Topic(topicName)
	exists, err = t.Exists(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "could not check if topic %s exists", topicName)
	}

	if !exists && s.config.DoNotCreateTopicIfMissing {
		return nil, errors.Wrap(ErrTopicDoesNotExist, topicName)
	}

	if !exists {
		t, err = s.client.CreateTopic(ctx, topicName)
		if err != nil {
			return nil, errors.Wrap(err, "could not create topic for subscription")
		}
	}

	config := s.config.SubscriptionConfig
	config.Topic = t
	sub, err = s.client.CreateSubscription(ctx, subscriptionName, config)
	sub.ReceiveSettings = s.config.ReceiveSettings

	return sub, err
}