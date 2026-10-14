package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
	"github.com/ordinary-s/rc_ordinary-s/internal/config"
)

type Event struct {
	EventID string `json:"event_id"`
	TaskID  string `json:"task_id"`
	TraceID string `json:"trace_id"`
}

type ProcessFunc func(context.Context, Event, int) error

type Client struct {
	cfg      config.Config
	logger   *slog.Logger
	producer rocketmq.Producer
	consumer rocketmq.PushConsumer
	started  atomic.Bool
}

func New(cfg config.Config, logger *slog.Logger) (*Client, error) {
	address, err := resolveNameServer(cfg.NameServer)
	if err != nil {
		return nil, err
	}
	resolver := primitive.NewPassthroughResolver([]string{address})
	p, err := rocketmq.NewProducer(
		producer.WithGroupName(cfg.ProducerGroup),
		producer.WithNsResolver(resolver),
		producer.WithRetry(0),
		producer.WithSendMsgTimeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("create RocketMQ producer: %w", err)
	}
	c, err := rocketmq.NewPushConsumer(
		consumer.WithGroupName(cfg.ConsumerGroup),
		consumer.WithNsResolver(resolver),
		consumer.WithConsumerModel(consumer.Clustering),
		consumer.WithConsumeMessageBatchMaxSize(1),
		consumer.WithPullBatchSize(1),
		consumer.WithConsumeGoroutineNums(cfg.ConsumerConcurrency),
		consumer.WithMaxReconsumeTimes(cfg.MaxReconsume),
		consumer.WithConsumeTimeout(cfg.MQConsumeTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("create RocketMQ consumer: %w", err)
	}
	return &Client{cfg: cfg, logger: logger, producer: p, consumer: c}, nil
}

func resolveNameServer(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("parse RocketMQ NameServer address: %w", err)
	}
	if net.ParseIP(host) != nil {
		return address, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", fmt.Errorf("resolve RocketMQ NameServer service %q: %w", host, err)
	}
	for _, ip := range ips {
		if ipv4 := ip.To4(); ipv4 != nil {
			return net.JoinHostPort(ipv4.String(), port), nil
		}
	}
	if len(ips) > 0 {
		return net.JoinHostPort(ips[0].String(), port), nil
	}
	return "", fmt.Errorf("resolve RocketMQ NameServer service %q: no IP address returned", host)
}

func (c *Client) Start(runCtx context.Context, process ProcessFunc) error {
	if err := c.producer.Start(); err != nil {
		return fmt.Errorf("start RocketMQ producer: %w", err)
	}
	if err := c.consumer.Subscribe(c.cfg.Topic, consumer.MessageSelector{}, func(ctx context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
		if len(messages) != 1 {
			return consumer.ConsumeRetryLater, fmt.Errorf("SDK delivered %d messages despite batch limit 1", len(messages))
		}
		message := messages[0]
		var event Event
		if err := json.Unmarshal(message.Body, &event); err != nil {
			return consumer.ConsumeRetryLater, fmt.Errorf("decode RocketMQ event: %w", err)
		}
		if event.EventID == "" || event.TaskID == "" || event.TraceID == "" {
			return consumer.ConsumeRetryLater, errors.New("RocketMQ event is missing a required identifier")
		}
		workCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(runCtx, cancel)
		defer func() {
			stop()
			cancel()
		}()
		if err := process(workCtx, event, int(message.ReconsumeTimes)); err != nil {
			return consumer.ConsumeRetryLater, err
		}
		return consumer.ConsumeSuccess, nil
	}); err != nil {
		_ = c.producer.Shutdown()
		return fmt.Errorf("subscribe RocketMQ topic %s: %w", c.cfg.Topic, err)
	}
	if err := c.consumer.Start(); err != nil {
		_ = c.consumer.Shutdown()
		_ = c.producer.Shutdown()
		return fmt.Errorf("start RocketMQ consumer: %w", err)
	}
	c.started.Store(true)
	return nil
}

func (c *Client) Publish(ctx context.Context, event Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode RocketMQ event: %w", err)
	}
	if len(body) > 64*1024 {
		return fmt.Errorf("RocketMQ event exceeds the 64 KiB application limit")
	}
	message := primitive.NewMessage(c.cfg.Topic, body)
	message.WithKeys([]string{event.EventID})
	result, err := c.producer.SendSync(ctx, message)
	if err != nil {
		return fmt.Errorf("send RocketMQ event %s: %w", event.EventID, err)
	}
	if result == nil || result.Status != primitive.SendOK {
		return fmt.Errorf("RocketMQ did not confirm event %s: status=%v", event.EventID, result)
	}
	return nil
}

func (c *Client) Ready(ctx context.Context, brokerAddress string) error {
	if !c.started.Load() {
		return errors.New("RocketMQ clients are not started")
	}
	for _, address := range []string{c.cfg.NameServer, brokerAddress} {
		dialer := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("RocketMQ endpoint %s unavailable: %w", address, err)
		}
		_ = conn.Close()
	}
	return nil
}

func (c *Client) Shutdown() error {
	c.started.Store(false)
	consumerErr := c.consumer.Shutdown()
	producerErr := c.producer.Shutdown()
	return errors.Join(consumerErr, producerErr)
}
