package kafka

import (
	"testing"
	"time"
)

func TestSubtreeMessage_EncodeDecode(t *testing.T) {
	msg := &SubtreeMessage{
		Hash:       "subtree-hash-123",
		DataHubURL: "https://datahub.example.com/subtree/123",
		PeerID:     "peer1",
		ClientName: "teranode-v1",
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeSubtreeMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.Hash != msg.Hash {
		t.Errorf("hash mismatch")
	}
	if decoded.DataHubURL != msg.DataHubURL {
		t.Errorf("dataHubUrl mismatch")
	}
	if decoded.PeerID != msg.PeerID {
		t.Errorf("peerId mismatch")
	}
	if decoded.ClientName != msg.ClientName {
		t.Errorf("clientName mismatch")
	}
}

func TestBlockMessage_EncodeDecode(t *testing.T) {
	msg := &BlockMessage{
		Hash:       "blockhash123",
		Height:     200,
		Header:     "0100000000000000",
		Coinbase:   "01000000010000",
		DataHubURL: "https://datahub.example.com/block/123",
		PeerID:     "peer2",
		ClientName: "teranode-v1",
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeBlockMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.Hash != msg.Hash {
		t.Errorf("hash mismatch")
	}
	if decoded.Height != 200 {
		t.Errorf("height mismatch")
	}
	if decoded.Header != msg.Header {
		t.Errorf("header mismatch")
	}
	if decoded.Coinbase != msg.Coinbase {
		t.Errorf("coinbase mismatch")
	}
	if decoded.DataHubURL != msg.DataHubURL {
		t.Errorf("dataHubUrl mismatch")
	}
}

func TestSubtreeWorkMessage_EncodeDecode(t *testing.T) {
	msg := &SubtreeWorkMessage{
		BlockHash:   "blockhash789",
		BlockHeight: 850000,
		SubtreeHash: "subtree-hash-456",
		DataHubURL:  "https://datahub.example.com/subtree/456",
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeSubtreeWorkMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.BlockHash != msg.BlockHash {
		t.Errorf("blockHash mismatch: got %s", decoded.BlockHash)
	}
	if decoded.BlockHeight != msg.BlockHeight {
		t.Errorf("blockHeight mismatch: got %d", decoded.BlockHeight)
	}
	if decoded.SubtreeHash != msg.SubtreeHash {
		t.Errorf("subtreeHash mismatch: got %s", decoded.SubtreeHash)
	}
	if decoded.DataHubURL != msg.DataHubURL {
		t.Errorf("dataHubUrl mismatch: got %s", decoded.DataHubURL)
	}
}

func TestStumpsMessage_EncodeDecode(t *testing.T) {
	msg := &StumpsMessage{
		CallbackURL: "https://example.com/cb",
		TxID:        "txid1",
		StatusType:  StatusSeenOnNetwork,
		RetryCount:  2,
		NextRetryAt: time.Now().Add(30 * time.Second).Truncate(time.Millisecond),
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeStumpsMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.CallbackURL != msg.CallbackURL {
		t.Errorf("callback URL mismatch")
	}
	if decoded.StatusType != StatusSeenOnNetwork {
		t.Errorf("status type mismatch")
	}
	if decoded.RetryCount != 2 {
		t.Errorf("retry count mismatch")
	}
}

func TestStumpsMessage_StumpRefs_EncodeDecode(t *testing.T) {
	msg := &StumpsMessage{
		CallbackURL: "https://example.com/cb",
		TxIDs:       []string{"txid1", "txid2", "txid3"},
		StumpRefs:   []string{"subtree-hash-a", "subtree-hash-b"},
		StatusType:  StatusMined,
		BlockHash:   "blockhash123",
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeStumpsMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if len(decoded.StumpRefs) != 2 {
		t.Fatalf("expected 2 StumpRefs, got %d", len(decoded.StumpRefs))
	}
	if decoded.StumpRefs[0] != "subtree-hash-a" || decoded.StumpRefs[1] != "subtree-hash-b" {
		t.Errorf("StumpRefs mismatch: got %v", decoded.StumpRefs)
	}
	if len(decoded.TxIDs) != 3 {
		t.Errorf("expected 3 TxIDs, got %d", len(decoded.TxIDs))
	}
	if decoded.StumpRef != "" {
		t.Errorf("expected empty singular StumpRef, got %s", decoded.StumpRef)
	}
}

func TestStumpsMessage_SingularStumpRef_EncodeDecode(t *testing.T) {
	msg := &StumpsMessage{
		CallbackURL: "https://example.com/cb",
		TxIDs:       []string{"txid1"},
		StumpRef:    "subtree-hash-single",
		StatusType:  StatusMined,
		BlockHash:   "blockhash456",
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeStumpsMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.StumpRef != "subtree-hash-single" {
		t.Errorf("StumpRef mismatch: got %s", decoded.StumpRef)
	}
	if len(decoded.StumpRefs) != 0 {
		t.Errorf("expected empty StumpRefs, got %v", decoded.StumpRefs)
	}
}

func TestBlockProcessedMessage_EncodeDecode(t *testing.T) {
	msg := &StumpsMessage{
		CallbackURL: "https://arcade.example.com/callback",
		StatusType:  StatusBlockProcessed,
		BlockHash:   "000000000000000003a2d78e5f7c9012",
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeStumpsMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.StatusType != StatusBlockProcessed {
		t.Errorf("expected BLOCK_PROCESSED, got %s", decoded.StatusType)
	}
	if decoded.BlockHash != msg.BlockHash {
		t.Errorf("blockHash mismatch: got %s", decoded.BlockHash)
	}
	if decoded.CallbackURL != msg.CallbackURL {
		t.Errorf("callbackURL mismatch: got %s", decoded.CallbackURL)
	}
	if decoded.TxID != "" {
		t.Errorf("expected empty txid, got %s", decoded.TxID)
	}
	if len(decoded.TxIDs) != 0 {
		t.Errorf("expected empty txids, got %v", decoded.TxIDs)
	}
	if len(decoded.StumpData) != 0 {
		t.Errorf("expected empty stumpData, got %v", decoded.StumpData)
	}
}
