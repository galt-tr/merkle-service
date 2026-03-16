package config

import (
	"fmt"
	"os"

	"github.com/spf13/viper"
)

// Config holds all configuration for the merkle-service.
type Config struct {
	Mode      string          `yaml:"mode"      mapstructure:"mode"`
	API       APIConfig       `yaml:"api"       mapstructure:"api"`
	Aerospike AerospikeConfig `yaml:"aerospike" mapstructure:"aerospike"`
	Kafka     KafkaConfig     `yaml:"kafka"     mapstructure:"kafka"`
	P2P       P2PConfig       `yaml:"p2p"       mapstructure:"p2p"`
	Subtree   SubtreeConfig   `yaml:"subtree"   mapstructure:"subtree"`
	Block     BlockConfig     `yaml:"block"     mapstructure:"block"`
	Callback  CallbackConfig  `yaml:"callback"  mapstructure:"callback"`
	BlobStore BlobStoreConfig `yaml:"blobStore" mapstructure:"blobstore"`
}

// APIConfig holds HTTP API configuration.
type APIConfig struct {
	Port int `yaml:"port" mapstructure:"port"`
}

// AerospikeConfig holds Aerospike connection configuration.
type AerospikeConfig struct {
	Host        string `yaml:"host"        mapstructure:"host"`
	Port        int    `yaml:"port"        mapstructure:"port"`
	Namespace   string `yaml:"namespace"   mapstructure:"namespace"`
	SetName     string `yaml:"setName"     mapstructure:"setname"`
	SeenSet     string `yaml:"seenSet"     mapstructure:"seenset"`
	MaxRetries  int    `yaml:"maxRetries"  mapstructure:"maxretries"`
	RetryBaseMs int    `yaml:"retryBaseMs" mapstructure:"retrybasems"`
}

// KafkaConfig holds Kafka connection configuration.
type KafkaConfig struct {
	Brokers        []string `yaml:"brokers"        mapstructure:"brokers"`
	SubtreeTopic   string   `yaml:"subtreeTopic"   mapstructure:"subtreetopic"`
	BlockTopic     string   `yaml:"blockTopic"     mapstructure:"blocktopic"`
	StumpsTopic    string   `yaml:"stumpsTopic"    mapstructure:"stumpstopic"`
	StumpsDLQTopic string   `yaml:"stumpsDlqTopic" mapstructure:"stumpsdlqtopic"`
	ConsumerGroup  string   `yaml:"consumerGroup"  mapstructure:"consumergroup"`
}

// P2PConfig holds peer-to-peer network configuration.
type P2PConfig struct {
	Network         string   `yaml:"network"        mapstructure:"network"`
	Name            string   `yaml:"name"           mapstructure:"name"`
	PrivateKey      string   `yaml:"privateKey"     mapstructure:"privatekey"`
	PeerCacheDir    string   `yaml:"peerCacheDir"   mapstructure:"peercachedir"`
	Port            int      `yaml:"port"           mapstructure:"port"`
	AnnounceAddrs   []string `yaml:"announceAddrs"  mapstructure:"announceaddrs"`
	BootstrapPeers  []string `yaml:"bootstrapPeers" mapstructure:"bootstrappeers"`
	SubtreeTopic    string   `yaml:"subtreeTopic"   mapstructure:"subtreetopic"`
	BlockTopic      string   `yaml:"blockTopic"     mapstructure:"blocktopic"`
	DHTMode         string   `yaml:"dhtMode"        mapstructure:"dhtmode"`
	EnableNAT       bool     `yaml:"enableNat"      mapstructure:"enablenat"`
	EnableMDNS      bool     `yaml:"enableMdns"     mapstructure:"enablemdns"`
	AllowPrivateIPs bool     `yaml:"allowPrivateIps" mapstructure:"allowprivateips"`
}

// SubtreeConfig holds subtree processing configuration.
type SubtreeConfig struct {
	StorageMode string `yaml:"storageMode" mapstructure:"storagemode"`
	DAHOffset   int    `yaml:"dahOffset"   mapstructure:"dahoffset"`
	CacheMaxMB  int    `yaml:"cacheMaxMB"  mapstructure:"cachemaxmb"`
}

// BlockConfig holds block processing configuration.
type BlockConfig struct {
	WorkerPoolSize int `yaml:"workerPoolSize" mapstructure:"workerpoolsize"`
	PostMineTTLSec int `yaml:"postMineTTLSec" mapstructure:"postminettlsec"`
}

// CallbackConfig holds callback delivery configuration.
type CallbackConfig struct {
	MaxRetries     int `yaml:"maxRetries"     mapstructure:"maxretries"`
	BackoffBaseSec int `yaml:"backoffBaseSec" mapstructure:"backoffbasesec"`
	TimeoutSec     int `yaml:"timeoutSec"     mapstructure:"timeoutsec"`
	SeenThreshold  int `yaml:"seenThreshold"  mapstructure:"seenthreshold"`
}

// BlobStoreConfig holds blob store configuration.
type BlobStoreConfig struct {
	URL string `yaml:"url" mapstructure:"url"`
}

// registerDefaults sets all default values in the Viper instance.
func registerDefaults(v *viper.Viper) {
	// General
	v.SetDefault("mode", "all-in-one")

	// API
	v.SetDefault("api.port", 8080)

	// Aerospike
	v.SetDefault("aerospike.host", "localhost")
	v.SetDefault("aerospike.port", 3000)
	v.SetDefault("aerospike.namespace", "merkle")
	v.SetDefault("aerospike.setname", "registrations")
	v.SetDefault("aerospike.seenset", "seen_counters")
	v.SetDefault("aerospike.maxretries", 3)
	v.SetDefault("aerospike.retrybasems", 100)

	// Kafka
	v.SetDefault("kafka.brokers", []string{"localhost:9092"})
	v.SetDefault("kafka.subtreetopic", "subtree")
	v.SetDefault("kafka.blocktopic", "block")
	v.SetDefault("kafka.stumpstopic", "stumps")
	v.SetDefault("kafka.stumpsdlqtopic", "stumps-dlq")
	v.SetDefault("kafka.consumergroup", "merkle-service")

	// P2P
	v.SetDefault("p2p.network", "mainnet")
	v.SetDefault("p2p.name", "merkle-service")
	v.SetDefault("p2p.port", 9906)
	v.SetDefault("p2p.bootstrappeers", []string{})
	v.SetDefault("p2p.subtreetopic", "subtree")
	v.SetDefault("p2p.blocktopic", "block")
	v.SetDefault("p2p.dhtmode", "off")
	v.SetDefault("p2p.enablenat", false)
	v.SetDefault("p2p.enablemdns", false)
	v.SetDefault("p2p.allowprivateips", false)

	// Subtree
	v.SetDefault("subtree.storagemode", "realtime")
	v.SetDefault("subtree.dahoffset", 1)
	v.SetDefault("subtree.cachemaxmb", 64)

	// Block
	v.SetDefault("block.workerpoolsize", 16)
	v.SetDefault("block.postminettlsec", 1800)

	// Callback
	v.SetDefault("callback.maxretries", 5)
	v.SetDefault("callback.backoffbasesec", 30)
	v.SetDefault("callback.timeoutsec", 10)
	v.SetDefault("callback.seenthreshold", 3)

	// BlobStore
	v.SetDefault("blobstore.url", "file:///tmp/merkle-subtrees")
}

// bindEnvVars explicitly binds environment variable names to Viper keys.
// This handles cases where the automatic dot-to-underscore mapping doesn't
// produce the expected env var name.
func bindEnvVars(v *viper.Viper) {
	// The general pattern is: key "section.field" → env "SECTION_FIELD"
	// Viper's AutomaticEnv with the replacer handles most of these,
	// but we bind explicitly for clarity and to ensure correct mapping.

	bindings := map[string]string{
		// General
		"mode": "MODE",

		// API
		"api.port": "API_PORT",

		// Aerospike
		"aerospike.host":        "AEROSPIKE_HOST",
		"aerospike.port":        "AEROSPIKE_PORT",
		"aerospike.namespace":   "AEROSPIKE_NAMESPACE",
		"aerospike.setname":     "AEROSPIKE_SET",
		"aerospike.seenset":     "AEROSPIKE_SEEN_SET",
		"aerospike.maxretries":  "AEROSPIKE_MAX_RETRIES",
		"aerospike.retrybasems": "AEROSPIKE_RETRY_BASE_MS",

		// Kafka
		"kafka.brokers":        "KAFKA_BROKERS",
		"kafka.subtreetopic":   "KAFKA_SUBTREE_TOPIC",
		"kafka.blocktopic":     "KAFKA_BLOCK_TOPIC",
		"kafka.stumpstopic":    "KAFKA_STUMPS_TOPIC",
		"kafka.stumpsdlqtopic": "KAFKA_STUMPS_DLQ_TOPIC",
		"kafka.consumergroup":  "KAFKA_CONSUMER_GROUP",

		// P2P
		"p2p.network":         "P2P_NETWORK",
		"p2p.name":            "P2P_NAME",
		"p2p.privatekey":      "P2P_PRIVATE_KEY",
		"p2p.peercachedir":    "P2P_PEER_CACHE_DIR",
		"p2p.port":            "P2P_PORT",
		"p2p.announceaddrs":   "P2P_ANNOUNCE_ADDRS",
		"p2p.bootstrappeers":  "P2P_BOOTSTRAP_PEERS",
		"p2p.subtreetopic":    "P2P_SUBTREE_TOPIC",
		"p2p.blocktopic":      "P2P_BLOCK_TOPIC",
		"p2p.dhtmode":         "P2P_DHT_MODE",
		"p2p.enablenat":       "P2P_ENABLE_NAT",
		"p2p.enablemdns":      "P2P_ENABLE_MDNS",
		"p2p.allowprivateips": "P2P_ALLOW_PRIVATE_IPS",

		// Subtree
		"subtree.storagemode": "SUBTREE_STORAGE_MODE",
		"subtree.dahoffset":   "SUBTREE_DAH_OFFSET",
		"subtree.cachemaxmb":  "SUBTREE_CACHE_MAX_MB",

		// Block
		"block.workerpoolsize": "BLOCK_WORKER_POOL_SIZE",
		"block.postminettlsec": "BLOCK_POST_MINE_TTL_SEC",

		// Callback
		"callback.maxretries":     "CALLBACK_MAX_RETRIES",
		"callback.backoffbasesec": "CALLBACK_BACKOFF_BASE_SEC",
		"callback.timeoutsec":     "CALLBACK_TIMEOUT_SEC",
		"callback.seenthreshold":  "CALLBACK_SEEN_THRESHOLD",

		// BlobStore
		"blobstore.url": "BLOB_STORE_URL",
	}

	for key, env := range bindings {
		_ = v.BindEnv(key, env)
	}
}

// Load reads configuration from defaults, YAML file, and environment variables.
// Priority order: env vars > YAML file > defaults.
func Load() (*Config, error) {
	v := viper.New()

	// Register defaults.
	registerDefaults(v)

	// Bind environment variables before reading config file.
	bindEnvVars(v)

	// Configure config file path.
	// Check CONFIG_FILE env var directly (not via Viper since it's not a config key).
	if configFile := os.Getenv("CONFIG_FILE"); configFile != "" {
		v.SetConfigFile(configFile)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
	}

	// Read config file (ignore file-not-found).
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// No config file found — use defaults + env vars only.
		} else if os.IsNotExist(err) {
			// Explicit CONFIG_FILE path doesn't exist — use defaults + env vars only.
		} else {
			// File exists but is invalid (parse error).
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
	}

	// Unmarshal into Config struct.
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &cfg, nil
}
