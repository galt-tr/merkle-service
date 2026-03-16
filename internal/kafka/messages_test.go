package kafka

import (
	"testing"
	"time"
)

func TestSubtreeMessage_EncodeDecode(t *testing.T) {
	msg := &SubtreeMessage{
		SubtreeID:   "sub123",
		TxIDs:       []string{"tx1", "tx2"},
		MerkleData:  []byte{0x01, 0x02},
		BlockHeight: 100,
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeSubtreeMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.SubtreeID != msg.SubtreeID {
		t.Errorf("subtree ID mismatch")
	}
	if len(decoded.TxIDs) != 2 {
		t.Errorf("expected 2 txids")
	}
	if decoded.BlockHeight != 100 {
		t.Errorf("block height mismatch")
	}
}

func TestBlockMessage_EncodeDecode(t *testing.T) {
	msg := &BlockMessage{
		BlockHash:   "blockhash123",
		BlockHeader: []byte{0x01},
		BlockHeight: 200,
		SubtreeRefs: []string{"sub1", "sub2"},
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := DecodeBlockMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.BlockHash != msg.BlockHash {
		t.Errorf("block hash mismatch")
	}
	if len(decoded.SubtreeRefs) != 2 {
		t.Errorf("expected 2 subtree refs")
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
