package stump

import (
	"encoding/binary"
)

// STUMP represents a Subtree Unified Merkle Path in BRC-0074 BUMP format.
type STUMP struct {
	BlockHeight uint64
	TreeHeight  uint8
	Paths       []PathLevel
}

// PathLevel represents one level of the merkle tree path.
type PathLevel struct {
	Leaves []Leaf
}

// Leaf represents a node at a given level in the merkle path.
type Leaf struct {
	Offset    uint64
	Hash      []byte // 32 bytes
	TxID      bool   // true if this is a txid (duplicate flag=0, txid flag=1)
	Duplicate bool   // true if hash should be duplicated from the complementary node
}

// Build constructs a STUMP for registered txids within a subtree merkle tree.
// merkleTree is the full subtree merkle tree as a flat array (level 0 = leaves = txids).
// registeredIndices maps leaf index to the txid at that position.
func Build(blockHeight uint64, merkleTree [][]byte, registeredIndices map[int]string) *STUMP {
	if len(registeredIndices) == 0 {
		return nil
	}

	treeHeight := calculateTreeHeight(len(merkleTree))
	stump := &STUMP{
		BlockHeight: blockHeight,
		TreeHeight:  treeHeight,
		Paths:       make([]PathLevel, treeHeight),
	}

	// Track which nodes we need at each level
	needed := make(map[int]bool)
	for idx := range registeredIndices {
		needed[idx] = true
	}

	levelSize := leafCount(merkleTree)
	offset := 0

	for level := uint8(0); level < treeHeight; level++ {
		pathLevel := PathLevel{}
		nextNeeded := make(map[int]bool)

		for idx := range needed {
			// Add the sibling node as a path element
			siblingIdx := idx ^ 1 // XOR with 1 to get sibling

			if siblingIdx < levelSize {
				leaf := Leaf{
					Offset: uint64(siblingIdx),
				}

				nodePos := offset + siblingIdx
				if nodePos < len(merkleTree) {
					if siblingIdx == idx {
						// Self-duplicate
						leaf.Duplicate = true
					} else {
						leaf.Hash = merkleTree[nodePos]
					}
				}

				// Check if sibling is also a registered txid at level 0
				if level == 0 {
					if _, isRegistered := registeredIndices[siblingIdx]; isRegistered {
						leaf.TxID = true
					}
				}

				pathLevel.Leaves = append(pathLevel.Leaves, leaf)
			}

			// Parent index for next level
			parentIdx := idx / 2
			nextNeeded[parentIdx] = true
		}

		stump.Paths[level] = pathLevel
		offset += levelSize
		levelSize = (levelSize + 1) / 2
		needed = nextNeeded
	}

	return stump
}

// Encode serializes a STUMP to BRC-0074 BUMP binary format.
func (s *STUMP) Encode() []byte {
	if s == nil {
		return nil
	}

	buf := make([]byte, 0, 256)

	// Block height (VarInt)
	buf = appendVarInt(buf, s.BlockHeight)

	// Tree height
	buf = append(buf, s.TreeHeight)

	// Path levels
	for _, level := range s.Paths {
		// Number of leaves at this level (VarInt)
		buf = appendVarInt(buf, uint64(len(level.Leaves)))

		for _, leaf := range level.Leaves {
			// Offset (VarInt)
			buf = appendVarInt(buf, leaf.Offset)

			// Flags byte: bit 0 = duplicate, bit 1 = txid
			var flags byte
			if leaf.Duplicate {
				flags |= 0x01
			}
			if leaf.TxID {
				flags |= 0x02
			}
			buf = append(buf, flags)

			// Hash (32 bytes, only if not duplicate)
			if !leaf.Duplicate {
				buf = append(buf, leaf.Hash...)
			}
		}
	}

	return buf
}

// MergeSTUMPs merges multiple STUMPs for the same subtree into one.
func MergeSTUMPs(stumps []*STUMP) *STUMP {
	if len(stumps) == 0 {
		return nil
	}
	if len(stumps) == 1 {
		return stumps[0]
	}

	merged := &STUMP{
		BlockHeight: stumps[0].BlockHeight,
		TreeHeight:  stumps[0].TreeHeight,
		Paths:       make([]PathLevel, stumps[0].TreeHeight),
	}

	for level := uint8(0); level < merged.TreeHeight; level++ {
		seen := make(map[uint64]Leaf)
		for _, s := range stumps {
			if int(level) < len(s.Paths) {
				for _, leaf := range s.Paths[level].Leaves {
					if _, exists := seen[leaf.Offset]; !exists {
						seen[leaf.Offset] = leaf
					}
				}
			}
		}

		leaves := make([]Leaf, 0, len(seen))
		for _, leaf := range seen {
			leaves = append(leaves, leaf)
		}
		merged.Paths[level] = PathLevel{Leaves: leaves}
	}

	return merged
}

// GroupByCallback groups registered txids by their callback URL.
// Returns map[callbackURL][]txid.
func GroupByCallback(registrations map[string][]string) map[string][]string {
	result := make(map[string][]string)
	for txid, callbacks := range registrations {
		for _, cb := range callbacks {
			result[cb] = append(result[cb], txid)
		}
	}
	return result
}

func calculateTreeHeight(nodeCount int) uint8 {
	if nodeCount <= 1 {
		return 1
	}
	height := uint8(0)
	n := nodeCount
	for n > 1 {
		n = (n + 1) / 2
		height++
	}
	return height
}

func leafCount(tree [][]byte) int {
	// For a binary tree stored as flat array, leaf count is roughly half + 1
	// This is a simplified calculation; actual implementation depends on tree format
	n := len(tree)
	leaves := 1
	for leaves < n {
		leaves *= 2
	}
	return leaves / 2
}

func appendVarInt(buf []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(buf, tmp[:n]...)
}

