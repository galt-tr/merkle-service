package p2p

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/bsv-blockchain/merkle-service/internal/config"
)

// loadOrGeneratePrivateKey loads an Ed25519 private key from config.
// Priority: (1) hex-encoded key from config.PrivateKey, (2) key file in PeerCacheDir, (3) auto-generate.
func loadOrGeneratePrivateKey(cfg config.P2PConfig) (crypto.PrivKey, error) {
	// 1. From config/env var (hex-encoded)
	if cfg.PrivateKey != "" {
		keyBytes, err := hex.DecodeString(cfg.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("invalid P2P_PRIVATE_KEY hex: %w", err)
		}
		privKey, err := crypto.UnmarshalEd25519PrivateKey(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid Ed25519 private key: %w", err)
		}
		return privKey, nil
	}

	// 2. From file in PeerCacheDir
	if cfg.PeerCacheDir != "" {
		keyPath := filepath.Join(cfg.PeerCacheDir, "p2p.key")
		keyBytes, err := os.ReadFile(keyPath)
		if err == nil {
			decoded, err := hex.DecodeString(string(keyBytes))
			if err == nil {
				privKey, err := crypto.UnmarshalEd25519PrivateKey(decoded)
				if err == nil {
					return privKey, nil
				}
			}
		}
	}

	// 3. Auto-generate
	privKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate Ed25519 key: %w", err)
	}

	// Persist if PeerCacheDir is set
	if cfg.PeerCacheDir != "" {
		if err := os.MkdirAll(cfg.PeerCacheDir, 0700); err == nil {
			raw, err := crypto.MarshalPrivateKey(privKey)
			if err == nil {
				keyPath := filepath.Join(cfg.PeerCacheDir, "p2p.key")
				_ = os.WriteFile(keyPath, []byte(hex.EncodeToString(raw)), 0600)
			}
		}
	}

	return privKey, nil
}
