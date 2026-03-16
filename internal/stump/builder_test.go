package stump

import (
	"testing"
)

func TestGroupByCallback(t *testing.T) {
	registrations := map[string][]string{
		"txid1": {"http://cb1.com", "http://cb2.com"},
		"txid2": {"http://cb1.com"},
		"txid3": {"http://cb3.com"},
	}

	grouped := GroupByCallback(registrations)

	if len(grouped["http://cb1.com"]) != 2 {
		t.Errorf("cb1 should have 2 txids, got %d", len(grouped["http://cb1.com"]))
	}
	if len(grouped["http://cb2.com"]) != 1 {
		t.Errorf("cb2 should have 1 txid, got %d", len(grouped["http://cb2.com"]))
	}
	if len(grouped["http://cb3.com"]) != 1 {
		t.Errorf("cb3 should have 1 txid, got %d", len(grouped["http://cb3.com"]))
	}
}

func TestBuild_NilForEmptyIndices(t *testing.T) {
	tree := make([][]byte, 4)
	for i := range tree {
		tree[i] = make([]byte, 32)
	}

	result := Build(100, tree, map[int]string{})
	if result != nil {
		t.Error("expected nil for empty indices")
	}
}

func TestBuild_SingleTx(t *testing.T) {
	// Create a simple 2-leaf tree
	leaf0 := make([]byte, 32)
	leaf1 := make([]byte, 32)
	leaf0[0] = 0xAA
	leaf1[0] = 0xBB

	tree := [][]byte{leaf0, leaf1}
	indices := map[int]string{0: "txid0"}

	result := Build(100, tree, indices)
	if result == nil {
		t.Fatal("expected non-nil STUMP")
	}
	if result.BlockHeight != 100 {
		t.Errorf("expected block height 100, got %d", result.BlockHeight)
	}
}

func TestEncode_ProducesBinaryOutput(t *testing.T) {
	s := &STUMP{
		BlockHeight: 100,
		TreeHeight:  2,
		Paths: []PathLevel{
			{Leaves: []Leaf{{Offset: 1, Hash: make([]byte, 32)}}},
			{Leaves: []Leaf{{Offset: 0, Duplicate: true}}},
		},
	}

	encoded := s.Encode()
	if len(encoded) == 0 {
		t.Fatal("encoded STUMP should not be empty")
	}
	// First byte(s) should be block height VarInt (100 = 0x64)
	if encoded[0] != 0x64 {
		t.Errorf("expected first byte 0x64, got 0x%x", encoded[0])
	}
}

func TestMergeSTUMPs_Empty(t *testing.T) {
	result := MergeSTUMPs(nil)
	if result != nil {
		t.Error("expected nil for empty input")
	}
}

func TestMergeSTUMPs_Single(t *testing.T) {
	s := &STUMP{BlockHeight: 1, TreeHeight: 1, Paths: []PathLevel{{Leaves: []Leaf{{Offset: 0}}}}}
	result := MergeSTUMPs([]*STUMP{s})
	if result != s {
		t.Error("expected same STUMP returned for single input")
	}
}

func TestMergeSTUMPs_TwoSTUMPs(t *testing.T) {
	hashA := make([]byte, 32)
	hashA[0] = 0xAA
	hashB := make([]byte, 32)
	hashB[0] = 0xBB
	hashC := make([]byte, 32)
	hashC[0] = 0xCC

	s1 := &STUMP{
		BlockHeight: 500,
		TreeHeight:  2,
		Paths: []PathLevel{
			{Leaves: []Leaf{
				{Offset: 1, Hash: hashA},
			}},
			{Leaves: []Leaf{
				{Offset: 0, Duplicate: true},
			}},
		},
	}

	s2 := &STUMP{
		BlockHeight: 500,
		TreeHeight:  2,
		Paths: []PathLevel{
			{Leaves: []Leaf{
				{Offset: 0, Hash: hashB},
			}},
			{Leaves: []Leaf{
				{Offset: 1, Hash: hashC},
			}},
		},
	}

	merged := MergeSTUMPs([]*STUMP{s1, s2})
	if merged == nil {
		t.Fatal("expected non-nil merged STUMP")
	}
	if merged.BlockHeight != 500 {
		t.Errorf("BlockHeight: expected 500, got %d", merged.BlockHeight)
	}
	if merged.TreeHeight != 2 {
		t.Errorf("TreeHeight: expected 2, got %d", merged.TreeHeight)
	}

	// Level 0 should have both leaves (offset 0 from s2 and offset 1 from s1)
	if len(merged.Paths[0].Leaves) != 2 {
		t.Errorf("level 0: expected 2 leaves, got %d", len(merged.Paths[0].Leaves))
	}

	// Level 1 should also have both leaves (offset 0 from s1 and offset 1 from s2)
	if len(merged.Paths[1].Leaves) != 2 {
		t.Errorf("level 1: expected 2 leaves, got %d", len(merged.Paths[1].Leaves))
	}
}

func TestMergeSTUMPs_DeduplicatesSameOffset(t *testing.T) {
	hash := make([]byte, 32)
	hash[0] = 0xDD

	s1 := &STUMP{
		BlockHeight: 100,
		TreeHeight:  1,
		Paths: []PathLevel{
			{Leaves: []Leaf{{Offset: 3, Hash: hash}}},
		},
	}

	s2 := &STUMP{
		BlockHeight: 100,
		TreeHeight:  1,
		Paths: []PathLevel{
			{Leaves: []Leaf{{Offset: 3, Hash: hash}}},
		},
	}

	merged := MergeSTUMPs([]*STUMP{s1, s2})
	if merged == nil {
		t.Fatal("expected non-nil merged STUMP")
	}
	// The same offset should appear only once (deduplicated).
	if len(merged.Paths[0].Leaves) != 1 {
		t.Errorf("expected 1 leaf at level 0 (deduped), got %d", len(merged.Paths[0].Leaves))
	}
	if merged.Paths[0].Leaves[0].Offset != 3 {
		t.Errorf("expected offset 3, got %d", merged.Paths[0].Leaves[0].Offset)
	}
}

func TestBuild_MultipleTxIds(t *testing.T) {
	// Build a 4-leaf merkle tree (flat layout: 4 leaves + 2 parents + 1 root = 7 nodes)
	tree := make([][]byte, 7)
	for i := range tree {
		tree[i] = make([]byte, 32)
		tree[i][0] = byte(i + 1) // distinct hashes
	}

	// Register two txids at leaf positions 0 and 2
	indices := map[int]string{
		0: "txid_a",
		2: "txid_c",
	}

	result := Build(200, tree, indices)
	if result == nil {
		t.Fatal("expected non-nil STUMP")
	}
	if result.BlockHeight != 200 {
		t.Errorf("BlockHeight: expected 200, got %d", result.BlockHeight)
	}
	// The tree height should be > 0 for a 7-node tree.
	if result.TreeHeight == 0 {
		t.Error("expected TreeHeight > 0")
	}
	// There should be path levels equal to TreeHeight.
	if len(result.Paths) != int(result.TreeHeight) {
		t.Errorf("expected %d path levels, got %d", result.TreeHeight, len(result.Paths))
	}
	// Level 0 should contain sibling nodes for the registered leaves.
	if len(result.Paths[0].Leaves) == 0 {
		t.Error("expected at least one leaf at level 0")
	}
}

func TestEncode_NilSTUMP(t *testing.T) {
	var s *STUMP
	encoded := s.Encode()
	if encoded != nil {
		t.Errorf("expected nil for nil STUMP encode, got %v", encoded)
	}
}

func TestEncode_DuplicateLeafOmitsHash(t *testing.T) {
	s := &STUMP{
		BlockHeight: 1,
		TreeHeight:  1,
		Paths: []PathLevel{
			{Leaves: []Leaf{{Offset: 0, Duplicate: true}}},
		},
	}

	encoded := s.Encode()
	if len(encoded) == 0 {
		t.Fatal("encoded STUMP should not be empty")
	}

	// Encoding for this STUMP:
	// - VarInt(1) for block height = 1 byte (0x01)
	// - 1 byte tree height = 0x01
	// - VarInt(1) for leaf count = 1 byte
	// - VarInt(0) for offset = 1 byte
	// - 1 byte flags (0x01 for duplicate)
	// No hash bytes follow a duplicate leaf.
	// Total: 5 bytes
	expectedLen := 5
	if len(encoded) != expectedLen {
		t.Errorf("expected %d bytes for duplicate-only leaf, got %d", expectedLen, len(encoded))
	}
}

func TestEncode_TxIDFlag(t *testing.T) {
	hash := make([]byte, 32)
	hash[0] = 0xFF

	s := &STUMP{
		BlockHeight: 10,
		TreeHeight:  1,
		Paths: []PathLevel{
			{Leaves: []Leaf{{Offset: 1, Hash: hash, TxID: true}}},
		},
	}

	encoded := s.Encode()
	if len(encoded) == 0 {
		t.Fatal("encoded STUMP should not be empty")
	}

	// Find the flags byte. Layout:
	// VarInt(10) = 1 byte, TreeHeight = 1 byte, VarInt(1) leaf count = 1 byte,
	// VarInt(1) offset = 1 byte, then flags byte at index 4.
	flagsByte := encoded[4]
	if flagsByte&0x02 == 0 {
		t.Errorf("expected TxID flag (bit 1) set in flags byte, got 0x%02x", flagsByte)
	}
	if flagsByte&0x01 != 0 {
		t.Errorf("expected Duplicate flag (bit 0) NOT set, got 0x%02x", flagsByte)
	}
}

func TestGroupByCallback_Empty(t *testing.T) {
	result := GroupByCallback(map[string][]string{})
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestGroupByCallback_SingleTxMultipleCallbacks(t *testing.T) {
	registrations := map[string][]string{
		"txid1": {"http://a.com", "http://b.com", "http://c.com"},
	}
	result := GroupByCallback(registrations)
	if len(result) != 3 {
		t.Errorf("expected 3 callback groups, got %d", len(result))
	}
	for _, cb := range []string{"http://a.com", "http://b.com", "http://c.com"} {
		txids := result[cb]
		if len(txids) != 1 || txids[0] != "txid1" {
			t.Errorf("callback %q: expected [txid1], got %v", cb, txids)
		}
	}
}

func TestCalculateTreeHeight(t *testing.T) {
	tests := []struct {
		nodeCount int
		expected  uint8
	}{
		{0, 1},
		{1, 1},
		{2, 1},
		{3, 2},
		{4, 2},
		{7, 3},
		{15, 4},
	}
	for _, tc := range tests {
		got := calculateTreeHeight(tc.nodeCount)
		if got != tc.expected {
			t.Errorf("calculateTreeHeight(%d): expected %d, got %d", tc.nodeCount, tc.expected, got)
		}
	}
}

func TestAppendVarInt(t *testing.T) {
	tests := []struct {
		value    uint64
		expected []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xAC, 0x02}},
	}
	for _, tc := range tests {
		buf := appendVarInt(nil, tc.value)
		if len(buf) != len(tc.expected) {
			t.Errorf("appendVarInt(%d): expected %d bytes, got %d", tc.value, len(tc.expected), len(buf))
			continue
		}
		for i := range buf {
			if buf[i] != tc.expected[i] {
				t.Errorf("appendVarInt(%d)[%d]: expected 0x%02X, got 0x%02X", tc.value, i, tc.expected[i], buf[i])
			}
		}
	}
}
