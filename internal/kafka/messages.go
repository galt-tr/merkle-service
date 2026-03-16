package kafka

import (
	"encoding/json"
	"time"
)

// StatusType represents the type of callback notification.
type StatusType string

const (
	StatusSeenOnNetwork  StatusType = "SEEN_ON_NETWORK"
	StatusSeenMultiNodes StatusType = "SEEN_MULTIPLE_NODES"
	StatusMined          StatusType = "MINED"
)

// SubtreeMessage represents a subtree announcement received from P2P.
type SubtreeMessage struct {
	Hash       string `json:"hash"`
	DataHubURL string `json:"dataHubUrl"`
	PeerID     string `json:"peerId"`
	ClientName string `json:"clientName"`
}

// BlockMessage represents a block announcement received from P2P.
type BlockMessage struct {
	Hash       string `json:"hash"`
	Height     uint32 `json:"height"`
	Header     string `json:"header"`
	Coinbase   string `json:"coinbase"`
	DataHubURL string `json:"dataHubUrl"`
	PeerID     string `json:"peerId"`
	ClientName string `json:"clientName"`
}

// StumpsMessage represents a callback notification (STUMP or status).
type StumpsMessage struct {
	CallbackURL string     `json:"callbackUrl"`
	TxID        string     `json:"txid,omitempty"`
	TxIDs       []string   `json:"txids,omitempty"`
	StumpData   []byte     `json:"stumpData,omitempty"`
	StatusType  StatusType `json:"statusType"`
	BlockHash   string     `json:"blockHash,omitempty"`
	SubtreeID   string     `json:"subtreeId,omitempty"`
	RetryCount  int        `json:"retryCount"`
	NextRetryAt time.Time  `json:"nextRetryAt,omitempty"`
}

func (m *SubtreeMessage) Encode() ([]byte, error) {
	return json.Marshal(m)
}

func DecodeSubtreeMessage(data []byte) (*SubtreeMessage, error) {
	var msg SubtreeMessage
	err := json.Unmarshal(data, &msg)
	return &msg, err
}

func (m *BlockMessage) Encode() ([]byte, error) {
	return json.Marshal(m)
}

func DecodeBlockMessage(data []byte) (*BlockMessage, error) {
	var msg BlockMessage
	err := json.Unmarshal(data, &msg)
	return &msg, err
}

func (m *StumpsMessage) Encode() ([]byte, error) {
	return json.Marshal(m)
}

func DecodeStumpsMessage(data []byte) (*StumpsMessage, error) {
	var msg StumpsMessage
	err := json.Unmarshal(data, &msg)
	return &msg, err
}
