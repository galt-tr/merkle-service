package p2p

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sync"
	"time"

	p2pMessageBus "github.com/bsv-blockchain/go-p2p-message-bus"
	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/service"
)

const (
	// maxPublishRetries is the maximum number of retries for Kafka publish failures.
	maxPublishRetries = 5
	// baseRetryDelay is the initial delay between publish retries.
	baseRetryDelay = 500 * time.Millisecond
)

// slogAdapter adapts *slog.Logger to the logger interface expected by go-p2p-message-bus.
type slogAdapter struct {
	logger *slog.Logger
}

func (s *slogAdapter) Infof(format string, args ...any)  { s.logger.Info(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Debugf(format string, args ...any) { s.logger.Debug(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Warnf(format string, args ...any)  { s.logger.Warn(fmt.Sprintf(format, args...)) }
func (s *slogAdapter) Errorf(format string, args ...any) { s.logger.Error(fmt.Sprintf(format, args...)) }

// Client is a P2P client service that connects to the Teranode P2P network
// via go-p2p-message-bus, subscribes to subtree and block topics, and publishes
// received messages to Kafka.
type Client struct {
	service.BaseService

	cfg       config.P2PConfig
	p2pClient p2pMessageBus.Client
	privKey   crypto.PrivKey

	subtreeProducer *kafka.Producer
	blockProducer   *kafka.Producer

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.RWMutex
	connected bool
	peerCount int
}

// buildTopicName returns a network-namespaced topic name in the form "{network}-{suffix}".
func buildTopicName(network, suffix string) string {
	return network + "-" + suffix
}

// buildProtocolVersion returns the protocol version string for the given network.
func buildProtocolVersion(network string) string {
	return "/teranode/bitcoin/" + network + "/1.0.0"
}

// NewClient creates a new P2P client with the given configuration and Kafka producers.
func NewClient(
	cfg config.P2PConfig,
	subtreeProducer *kafka.Producer,
	blockProducer *kafka.Producer,
	logger *slog.Logger,
) *Client {
	c := &Client{
		cfg:             cfg,
		subtreeProducer: subtreeProducer,
		blockProducer:   blockProducer,
	}
	c.InitBase("p2p-client")
	if logger != nil {
		c.Logger = logger
	}
	return c
}

// Init validates the configuration, loads/generates the private key, and
// prepares the client for startup.
func (c *Client) Init(_ interface{}) error {
	if c.subtreeProducer == nil {
		return fmt.Errorf("subtree kafka producer is required")
	}
	if c.blockProducer == nil {
		return fmt.Errorf("block kafka producer is required")
	}
	if c.cfg.SubtreeTopic == "" {
		return fmt.Errorf("P2P subtree topic is required")
	}
	if c.cfg.BlockTopic == "" {
		return fmt.Errorf("P2P block topic is required")
	}

	// Load or generate the Ed25519 private key.
	privKey, err := loadOrGeneratePrivateKey(c.cfg)
	if err != nil {
		return fmt.Errorf("failed to load/generate private key: %w", err)
	}
	c.privKey = privKey

	c.Logger.Info("p2p client initialized",
		"subtreeTopic", c.cfg.SubtreeTopic,
		"blockTopic", c.cfg.BlockTopic,
		"bootstrapPeers", len(c.cfg.BootstrapPeers),
	)
	return nil
}

// Start creates the p2p message bus client, subscribes to topics, and begins
// processing incoming messages.
func (c *Client) Start(ctx context.Context) error {
	ctx, c.cancel = context.WithCancel(ctx)

	network := c.cfg.Network
	if network == "" {
		network = "mainnet"
	}

	// Build the peer cache file path if PeerCacheDir is configured.
	var peerCacheFile string
	if c.cfg.PeerCacheDir != "" {
		peerCacheFile = filepath.Join(c.cfg.PeerCacheDir, "peers.json")
	}

	// Build the p2p message bus config.
	p2pCfg := p2pMessageBus.Config{
		Name:            c.cfg.Name,
		PrivateKey:      c.privKey,
		Port:            c.cfg.Port,
		AnnounceAddrs:   c.cfg.AnnounceAddrs,
		BootstrapPeers:  c.cfg.BootstrapPeers,
		ProtocolVersion: buildProtocolVersion(network),
		PeerCacheFile:   peerCacheFile,
		DHTMode:         c.cfg.DHTMode,
		EnableNAT:       c.cfg.EnableNAT,
		EnableMDNS:      c.cfg.EnableMDNS,
		AllowPrivateIPs: c.cfg.AllowPrivateIPs,
		Logger:          &slogAdapter{logger: c.Logger},
	}

	// Create the p2p message bus client.
	client, err := p2pMessageBus.NewClient(p2pCfg)
	if err != nil {
		return fmt.Errorf("failed to create p2p message bus client: %w", err)
	}
	c.p2pClient = client

	c.Logger.Info("p2p message bus client created",
		"peerID", client.GetID(),
		"network", network,
	)

	// Build network-aware topic names.
	subtreeTopicName := buildTopicName(network, c.cfg.SubtreeTopic)
	blockTopicName := buildTopicName(network, c.cfg.BlockTopic)

	// Subscribe to topics.
	subtreeCh := c.p2pClient.Subscribe(subtreeTopicName)
	blockCh := c.p2pClient.Subscribe(blockTopicName)

	// Start message processing goroutines.
	c.wg.Add(2)
	go c.processMessages(ctx, subtreeCh, c.handleSubtreeMessage, "subtree")
	go c.processMessages(ctx, blockCh, c.handleBlockMessage, "block")

	c.SetStarted(true)
	c.setConnected(true)

	c.Logger.Info("p2p client started",
		"subtreeTopic", subtreeTopicName,
		"blockTopic", blockTopicName,
	)
	return nil
}

// Stop gracefully shuts down the P2P client, closing the message bus and
// waiting for goroutines to complete.
func (c *Client) Stop() error {
	c.Logger.Info("stopping p2p client")

	if c.cancel != nil {
		c.cancel()
	}

	// Wait for goroutines to finish with a timeout.
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		c.Logger.Warn("timed out waiting for p2p goroutines to stop")
	}

	if c.p2pClient != nil {
		if err := c.p2pClient.Close(); err != nil {
			c.Logger.Error("error closing p2p message bus client", "error", err)
		}
	}

	c.SetStarted(false)
	c.setConnected(false)

	c.Logger.Info("p2p client stopped")
	return nil
}

// Health returns the current health status of the P2P client.
func (c *Client) Health() service.HealthStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Update peer count from the p2p client if available.
	peerCount := c.peerCount
	if c.p2pClient != nil {
		peerCount = len(c.p2pClient.GetPeers())
	}

	status := "healthy"
	details := map[string]string{
		"peerCount": fmt.Sprintf("%d", peerCount),
	}

	if !c.connected {
		status = "unhealthy"
		details["connection"] = "disconnected"
	} else {
		details["connection"] = "connected"
	}

	if peerCount == 0 {
		status = "degraded"
		details["peers"] = "no peers connected"
	}

	return service.HealthStatus{
		Name:    "p2p-client",
		Status:  status,
		Details: details,
	}
}

// processMessages reads messages from a subscription channel and dispatches
// them to the given handler function.
func (c *Client) processMessages(ctx context.Context, ch <-chan p2pMessageBus.Message, handler func([]byte), topicLabel string) {
	defer c.wg.Done()

	c.Logger.Info("starting message processing loop", "topic", topicLabel)

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				c.Logger.Info("message channel closed", "topic", topicLabel)
				return
			}
			handler(msg.Data)
		case <-ctx.Done():
			c.Logger.Info("message loop exiting: context cancelled", "topic", topicLabel)
			return
		}
	}
}

// handleSubtreeMessage deserializes a raw subtree message and publishes it to Kafka.
func (c *Client) handleSubtreeMessage(data []byte) {
	// Deserialize the P2P message. Currently using JSON encoding;
	// this will be replaced with protobuf when Teranode schemas are finalized.
	var subtreeMsg kafka.SubtreeMessage
	if err := json.Unmarshal(data, &subtreeMsg); err != nil {
		c.Logger.Error("failed to deserialize subtree message",
			"error", err,
			"dataLen", len(data),
		)
		return
	}

	c.Logger.Debug("received subtree message",
		"subtreeId", subtreeMsg.SubtreeID,
		"blockHeight", subtreeMsg.BlockHeight,
		"txCount", len(subtreeMsg.TxIDs),
	)

	encoded, err := subtreeMsg.Encode()
	if err != nil {
		c.Logger.Error("failed to encode subtree message for kafka",
			"subtreeId", subtreeMsg.SubtreeID,
			"error", err,
		)
		return
	}

	c.publishWithRetry(c.subtreeProducer, subtreeMsg.SubtreeID, encoded, "subtree")
}

// handleBlockMessage deserializes a raw block message and publishes it to Kafka.
func (c *Client) handleBlockMessage(data []byte) {
	// Deserialize the P2P message. Currently using JSON encoding;
	// this will be replaced with protobuf when Teranode schemas are finalized.
	var blockMsg kafka.BlockMessage
	if err := json.Unmarshal(data, &blockMsg); err != nil {
		c.Logger.Error("failed to deserialize block message",
			"error", err,
			"dataLen", len(data),
		)
		return
	}

	c.Logger.Debug("received block message",
		"blockHash", blockMsg.BlockHash,
		"blockHeight", blockMsg.BlockHeight,
		"subtreeRefs", len(blockMsg.SubtreeRefs),
	)

	encoded, err := blockMsg.Encode()
	if err != nil {
		c.Logger.Error("failed to encode block message for kafka",
			"blockHash", blockMsg.BlockHash,
			"error", err,
		)
		return
	}

	c.publishWithRetry(c.blockProducer, blockMsg.BlockHash, encoded, "block")
}

// publishWithRetry attempts to publish a message to Kafka with exponential backoff retries.
func (c *Client) publishWithRetry(producer *kafka.Producer, key string, value []byte, msgType string) {
	delay := baseRetryDelay

	for attempt := 1; attempt <= maxPublishRetries; attempt++ {
		err := producer.Publish(key, value)
		if err == nil {
			return
		}

		c.Logger.Error("kafka publish failed",
			"type", msgType,
			"key", key,
			"attempt", attempt,
			"maxAttempts", maxPublishRetries,
			"error", err,
		)

		if attempt == maxPublishRetries {
			c.Logger.Error("kafka publish exhausted all retries, message dropped",
				"type", msgType,
				"key", key,
				"valueLen", len(value),
			)
			return
		}

		time.Sleep(delay)
		delay = time.Duration(math.Min(float64(delay)*2, float64(10*time.Second)))
	}
}

// setConnected updates the connected state in a thread-safe manner.
func (c *Client) setConnected(connected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = connected
}
