// Quick smoke test for the transfer server/client pair.
// Run on spark01 to serve, then on spark02 to pull.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Borgels/mantlerd/internal/transfer"
	"github.com/Borgels/mantlerd/internal/types"
)

func main() {
	mode := flag.String("mode", "server", "server or client")
	secret := flag.String("secret", "test-secret-12345", "HMAC secret")
	machineID := flag.String("machine", "test-machine", "machine ID")
	peerAddr := flag.String("peer", "", "peer address for client mode")
	peerMachineID := flag.String("peer-machine", "peer", "peer machine ID (must match server -machine flag)")
	modelID := flag.String("model", "gemma3:27b", "model ID to transfer")
	flag.Parse()

	store := transfer.NewStore()
	secretFunc := func() string { return *secret }

	switch *mode {
	case "addrs":
		addrs := transfer.RankedTransferAddresses()
		fmt.Printf("Ranked transfer addresses (%d):\n", len(addrs))
		for i, a := range addrs {
			fmt.Printf("  [%d] %s\n", i+1, a)
		}

	case "server":
		server := transfer.NewServer(*machineID, store, secretFunc)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		log.Printf("Starting transfer server on port %d for machine %s", transfer.TransferPort, *machineID)
		// List available models
		available := store.ListAvailable()
		log.Printf("Available models: %d", len(available))
		for _, m := range available {
			log.Printf("  - %s (%s) digest=%s size=%dMB", m.ModelID, m.Runtime, m.Digest[:8], m.Size/(1024*1024))
		}
		if err := server.ListenAndServe(ctx); err != nil {
			log.Fatalf("server: %v", err)
		}

	case "client":
		if *peerAddr == "" {
			log.Fatal("need -peer address")
		}
		client := transfer.NewClient(*machineID, store, secretFunc)
		peers := []types.PeerHint{
			{
				MachineID:    *peerMachineID,
				Addresses:    []string{*peerAddr},
				TransferPort: transfer.TransferPort,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		log.Printf("Attempting to pull %q from %s", *modelID, *peerAddr)
		result, err := client.PullFromPeers(ctx, *modelID, "ollama", peers, func(received, total int64) {
			if total > 0 {
				fmt.Printf("\r%.1f MB / %.1f MB (%.0f%%)",
					float64(received)/1e6, float64(total)/1e6,
					float64(received)/float64(total)*100)
			} else {
				fmt.Printf("\r%.1f MB received", float64(received)/1e6)
			}
		})
		fmt.Println()
		if err != nil {
			log.Fatalf("pull failed: %v", err)
		}
		if result == nil {
			log.Fatal("no peer could serve the model")
		}
		log.Printf("Transfer complete: digest=%s bytes=%d source=%s",
			result.Digest[:8], result.Bytes, result.SourceMachineID)
		os.Exit(0)
	}
}
