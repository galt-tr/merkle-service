//go:build scale

package scale

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/bsv-blockchain/merkle-service/internal/datahub"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
)

// injectBlock publishes a BlockMessage to the Kafka block topic.
func injectBlock(manifest *Manifest, blockTopic string, producer *kafka.Producer, dataHubURL string) error {
	msg := &kafka.BlockMessage{
		Hash:       manifest.BlockHash,
		Height:     manifest.BlockHeight,
		DataHubURL: dataHubURL,
	}
	data, err := msg.Encode()
	if err != nil {
		return fmt.Errorf("encoding block message: %w", err)
	}
	if err := producer.Publish(manifest.BlockHash, data); err != nil {
		return fmt.Errorf("publishing block message: %w", err)
	}
	return nil
}

// startMockDataHub starts an HTTP server that serves block metadata and subtree data.
// It returns the server and its base URL.
func startMockDataHub(manifest *Manifest, subtreeData map[string][]byte) (*http.Server, string, error) {
	mux := http.NewServeMux()

	// Block metadata endpoint: /block/{hash}/json
	mux.HandleFunc("/block/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/json") {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		subtreeHashes := make([]string, len(manifest.Subtrees))
		for i, st := range manifest.Subtrees {
			subtreeHashes[i] = st.Hash
		}

		meta := datahub.BlockMetadata{
			Height:           manifest.BlockHeight,
			Subtrees:         subtreeHashes,
			TransactionCount: uint64(manifest.TotalTxids),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta)
	})

	// Subtree data endpoint: /subtree/{hash}
	mux.HandleFunc("/subtree/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hash := parts[len(parts)-1]
		data, ok := subtreeData[hash]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	})

	// Use port 0 for random available port.
	ln, err := listenOnFreePort()
	if err != nil {
		return nil, "", err
	}

	server := &http.Server{Handler: mux}
	go server.Serve(ln)

	return server, fmt.Sprintf("http://%s", ln.Addr().String()), nil
}

func listenOnFreePort() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
