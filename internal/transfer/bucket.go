package transfer

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

func bucketName(nodeID string) string {
	return "clawsynapse-transfer-" + nodeID
}

func (s *Service) ensureBucket(nodeID string) (nats.ObjectStore, error) {
	js := s.bus.JetStream()
	if js == nil {
		return nil, fmt.Errorf("jetstream not available")
	}

	name := bucketName(nodeID)

	store, err := js.ObjectStore(name)
	if err == nil {
		return store, nil
	}

	store, err = js.CreateObjectStore(&nats.ObjectStoreConfig{
		Bucket:  name,
		TTL:     s.ttl,
		Storage: nats.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("create bucket %s: %w", name, err)
	}
	return store, nil
}

func (s *Service) getBucket(nodeID string) (nats.ObjectStore, error) {
	js := s.bus.JetStream()
	if js == nil {
		return nil, fmt.Errorf("jetstream not available")
	}
	name := bucketName(nodeID)
	store, err := js.ObjectStore(name)
	if err != nil {
		return nil, fmt.Errorf("get bucket %s: %w", name, err)
	}
	return store, nil
}

func parseTTL(ttl string) time.Duration {
	if ttl == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return 24 * time.Hour
	}
	return d
}
