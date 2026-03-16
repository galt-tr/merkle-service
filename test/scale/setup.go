//go:build scale

package scale

import (
	"fmt"
	"log/slog"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"

	"github.com/bsv-blockchain/merkle-service/internal/store"
)

// preloadRegistrations loads all txid→callbackURL registrations into Aerospike.
func preloadRegistrations(manifest *Manifest, txids [][]byte, regStore *store.RegistrationStore, logger *slog.Logger) error {
	for _, arcade := range manifest.ArcadeInstances {
		for j := arcade.TxidStart; j < arcade.TxidEnd; j++ {
			// Convert raw hash to Bitcoin display order (reversed hex) matching chainhash.Hash.String().
			var h chainhash.Hash
			copy(h[:], txids[j])
			txidStr := h.String()

			if err := regStore.Add(txidStr, arcade.CallbackURL); err != nil {
				return fmt.Errorf("adding registration for txid index %d: %w", j, err)
			}
		}
		if (arcade.Index+1)%10 == 0 {
			logger.Info("pre-loaded registrations", "arcadeInstances", arcade.Index+1, "txids", (arcade.Index+1)*(arcade.TxidEnd-arcade.TxidStart))
		}
	}
	return nil
}

// preloadCallbackURLRegistry adds all callback URLs to the broadcast registry.
func preloadCallbackURLRegistry(manifest *Manifest, urlRegistry *store.CallbackURLRegistry) error {
	for _, arcade := range manifest.ArcadeInstances {
		if err := urlRegistry.Add(arcade.CallbackURL); err != nil {
			return fmt.Errorf("adding callback URL for arcade %d: %w", arcade.Index, err)
		}
	}
	return nil
}

// preloadSubtrees loads all subtree binary data into the subtree store.
func preloadSubtrees(manifest *Manifest, subtreeData map[string][]byte, subtreeStore *store.SubtreeStore) error {
	for _, st := range manifest.Subtrees {
		data, ok := subtreeData[st.Hash]
		if !ok {
			return fmt.Errorf("missing subtree data for hash %s", st.Hash)
		}
		if err := subtreeStore.StoreSubtree(st.Hash, data, uint64(manifest.BlockHeight)); err != nil {
			return fmt.Errorf("storing subtree %s: %w", st.Hash, err)
		}
	}
	return nil
}
