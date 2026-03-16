package block

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/stump"
)

// processSubtree processes a single subtree within a block.
func (p *Processor) processSubtree(
	ctx context.Context,
	blockMsg *kafka.BlockMessage,
	subtreeID string,
	subtreeIdx int,
	registeredTxIDs chan<- string,
) error {
	// Retrieve subtree data from blob store
	subtreeData, err := p.subtreeStore.GetSubtree(subtreeID)
	if err != nil {
		p.Logger.Error("failed to retrieve subtree", "subtreeId", subtreeID, "error", err)
		return nil // Skip this subtree, don't fail the block
	}

	// Parse subtree to extract txids and merkle tree
	// For now, assume subtree data is a JSON-encoded SubtreeMessage
	var subtreeMsg kafka.SubtreeMessage
	if err := json.Unmarshal(subtreeData, &subtreeMsg); err != nil {
		p.Logger.Error("failed to parse subtree data", "subtreeId", subtreeID, "error", err)
		return nil
	}

	txids := subtreeMsg.TxIDs
	if len(txids) == 0 {
		return nil
	}

	// Batch check registrations
	registrations, err := p.regStore.BatchGet(txids)
	if err != nil {
		p.Logger.Error("failed to check registrations", "subtreeId", subtreeID, "error", err)
		return nil
	}

	if len(registrations) == 0 {
		return nil
	}

	// Send registered txids for TTL update
	for txid := range registrations {
		select {
		case registeredTxIDs <- txid:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Build txid index map for STUMP construction
	txidIndex := make(map[int]string)
	for i, txid := range txids {
		if _, registered := registrations[txid]; registered {
			txidIndex[i] = txid
		}
	}

	// Handle coinbase placeholder for subtree 0
	isCoinbaseSubtree := subtreeIdx == 0

	// Group by callback URL
	callbackGroups := stump.GroupByCallback(registrations)

	// Build STUMP per callback URL
	// Reconstruct merkle tree from subtree data
	merkleTree := buildMerkleTree(txids, subtreeMsg.MerkleData)

	for callbackURL, cbTxIDs := range callbackGroups {
		// Build registered indices for this callback
		indices := make(map[int]string)
		for i, txid := range txids {
			for _, cbTxID := range cbTxIDs {
				if txid == cbTxID {
					// Skip coinbase entry (index 0 in subtree 0)
					if isCoinbaseSubtree && i == 0 {
						continue
					}
					indices[i] = txid
				}
			}
		}

		if len(indices) == 0 {
			continue
		}

		// Build STUMP
		s := stump.Build(blockMsg.BlockHeight, merkleTree, indices)
		if s == nil {
			continue
		}

		stumpData := s.Encode()

		// Publish STUMP to Kafka stumps topic
		stumpsMsg := &kafka.StumpsMessage{
			CallbackURL: callbackURL,
			TxIDs:       cbTxIDs,
			StumpData:   stumpData,
			StatusType:  kafka.StatusMined,
			BlockHash:   blockMsg.BlockHash,
			SubtreeID:   subtreeID,
		}

		data, err := stumpsMsg.Encode()
		if err != nil {
			p.Logger.Error("failed to encode STUMP message", "error", err)
			continue
		}

		if err := p.stumpsProducer.PublishWithHashKey(callbackURL, data); err != nil {
			p.Logger.Error("failed to publish STUMP", "callbackUrl", callbackURL, "error", err)
		}
	}

	return nil
}

// buildMerkleTree constructs a flat merkle tree from txids and merkle data.
// The tree is stored as a flat array where level 0 contains the leaves (txid hashes).
func buildMerkleTree(txids []string, merkleData []byte) [][]byte {
	// If we have pre-computed merkle data, use it
	if len(merkleData) > 0 {
		return parseMerkleData(merkleData)
	}

	// Otherwise, build from txids (simplified - in production this would use double-SHA256)
	leaves := make([][]byte, len(txids))
	for i, txid := range txids {
		hash, err := hex.DecodeString(txid)
		if err != nil {
			leaves[i] = make([]byte, 32)
		} else {
			leaves[i] = hash
		}
	}

	tree := make([][]byte, 0, len(leaves)*2)
	tree = append(tree, leaves...)

	// Build levels
	current := leaves
	for len(current) > 1 {
		var next [][]byte
		for i := 0; i < len(current); i += 2 {
			if i+1 < len(current) {
				next = append(next, hashPair(current[i], current[i+1]))
			} else {
				next = append(next, hashPair(current[i], current[i]))
			}
		}
		tree = append(tree, next...)
		current = next
	}

	return tree
}

func parseMerkleData(data []byte) [][]byte {
	// Parse pre-serialized merkle tree data
	// Each node is 32 bytes
	var nodes [][]byte
	for i := 0; i < len(data); i += 32 {
		end := i + 32
		if end > len(data) {
			break
		}
		node := make([]byte, 32)
		copy(node, data[i:end])
		nodes = append(nodes, node)
	}
	return nodes
}

func hashPair(a, b []byte) []byte {
	combined := make([]byte, 64)
	copy(combined[:32], a)
	copy(combined[32:], b)
	first := sha256.Sum256(combined)
	second := sha256.Sum256(first[:])
	return second[:]
}
