# Kubernetes Deployment

Deploy merkle-service as independent microservices on Kubernetes.

## Prerequisites

- **Kafka** cluster accessible within the cluster (default: `kafka-0.kafka.merkle-service.svc.cluster.local:9092`)
- **Aerospike** cluster accessible within the cluster (default: `aerospike.merkle-service.svc.cluster.local:3000`)
- **Kafka topics** created with appropriate partition counts:
  - `subtree` — P2P subtree announcements
  - `block` — block announcements
  - `stumps` — STUMP/callback status messages
  - `stumps-dlq` — dead-letter queue for failed callbacks
  - `subtree-work` — subtree work items for parallel processing (**partition count determines max subtree-worker replicas**)

## Deployment

```bash
# Create namespace and shared config
kubectl apply -f namespace.yaml
kubectl apply -f configmap.yaml

# Deploy services
kubectl apply -f p2p-client.yaml
kubectl apply -f block-processor.yaml
kubectl apply -f subtree-worker.yaml
kubectl apply -f callback-delivery.yaml
kubectl apply -f api-server.yaml
```

## Architecture

```
P2P Client (1 pod) → Kafka → Block Processor (1 pod) → subtree-work topic
                                                              ↓
                                              Subtree Workers (16-256 pods)
                                                              ↓
                                                   Aerospike STUMP cache
                                                              ↓
                                              stumps topic → Callback Delivery (4+ pods)
                                                              ↓
                                                        HTTP callbacks
```

## Services

| Service | Default Replicas | Scaling Limit | Notes |
|---------|-----------------|---------------|-------|
| `p2p-client` | 1 | 1 (singleton) | Maintains BSV P2P connection. Running >1 causes duplicate messages. |
| `block-processor` | 1 | block topic partitions | Blocks arrive ~1/10min. Extra replicas are standby. |
| `subtree-worker` | 16 | subtree-work topic partitions | Main scaling lever. Set topic partitions >= max replicas. |
| `callback-delivery` | 4 | stumps topic partitions | Scale based on callback throughput needs. |
| `api-server` | 2 | unlimited (stateless) | Handles `/watch` registrations and `/health`. |

## Scaling Subtree Workers

The subtree-worker pods form a Kafka consumer group. Each pod consumes a subset of partitions from the `subtree-work` topic. The maximum useful replica count equals the topic's partition count.

```bash
# Check current partition count
kafka-topics.sh --describe --topic subtree-work --bootstrap-server kafka:9092

# Increase partitions (cannot decrease)
kafka-topics.sh --alter --topic subtree-work --partitions 256 --bootstrap-server kafka:9092

# Scale workers to match
kubectl scale deployment subtree-worker -n merkle-service --replicas=256
```

## Environment Variables

Override any config value via environment variables. Key settings for K8s:

| Variable | Default | Description |
|----------|---------|-------------|
| `MODE` | `all-in-one` | Set to `microservice` in K8s (set in configmap) |
| `CALLBACK_STUMP_CACHE_MODE` | `memory` | Set to `aerospike` for cross-pod STUMP sharing |
| `CALLBACK_STUMP_CACHE_LRU_SIZE` | `1024` | Local LRU entries per delivery pod (~270 MB at 1024) |
| `CALLBACK_STUMP_CACHE_TTL_SEC` | `300` | STUMP cache TTL in seconds |
| `AEROSPIKE_HOST` | `localhost` | Aerospike service address |
| `AEROSPIKE_PORT` | `3000` | Aerospike client port |
| `AEROSPIKE_NAMESPACE` | `merkle` | Aerospike namespace |
| `AEROSPIKE_STUMP_CACHE_SET` | `stump_cache` | Aerospike set for distributed STUMP cache |
| `KAFKA_BROKERS` | `localhost:9092` | Comma-separated Kafka broker list |
| `KAFKA_SUBTREE_WORK_TOPIC` | `subtree-work` | Topic for subtree work fan-out |
| `KAFKA_CONSUMER_GROUP` | `merkle-service` | Kafka consumer group ID |

See `config.yaml` in the project root for the full list of configuration options.

## Resource Tuning

- **Subtree workers**: Each holds one STUMP (~271 KB) in memory during processing. The LRU cache adds up to `LRU_SIZE * 271 KB` memory usage. Default 1024 entries = ~270 MB.
- **Callback delivery**: Similar LRU memory usage. Increase `deliveryWorkers` and `maxConnsPerHost` for higher callback throughput.
- **Block processor**: Lightweight — mostly publishes Kafka messages. Low resource requirements.
