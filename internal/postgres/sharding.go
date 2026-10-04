package postgres

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// ShardRouter routes database operations to a specific DBCluster based on propertyID.
// This enables horizontal database sharding across independent PostgreSQL instances from ground level.
type ShardRouter interface {
	// GetShard returns the assigned DBCluster for the given propertyID.
	GetShard(propertyID uuid.UUID) *DBCluster
	// Default returns the default (primary) DBCluster.
	Default() *DBCluster
	// AllShards returns all distinct DBClusters managed by this router.
	AllShards() []*DBCluster
	// Close gracefully closes all managed DBClusters.
	Close()
}

// SingleShardRouter is the zero-overhead default ShardRouter for single-cluster deployments.
type SingleShardRouter struct {
	cluster *DBCluster
}

// NewSingleShardRouter wraps a single DBCluster.
func NewSingleShardRouter(cluster *DBCluster) *SingleShardRouter {
	return &SingleShardRouter{cluster: cluster}
}

func (r *SingleShardRouter) GetShard(_ uuid.UUID) *DBCluster {
	return r.cluster
}

func (r *SingleShardRouter) Default() *DBCluster {
	return r.cluster
}

func (r *SingleShardRouter) AllShards() []*DBCluster {
	return []*DBCluster{r.cluster}
}

func (r *SingleShardRouter) Close() {
	if r.cluster != nil {
		r.cluster.Close()
	}
}

// ConsistentHashShardRouter distributes properties across N database clusters using consistent hashing.
type ConsistentHashShardRouter struct {
	shards []*DBCluster
}

// NewConsistentHashShardRouter creates a sharded router with N clusters.
func NewConsistentHashShardRouter(shards ...*DBCluster) (*ConsistentHashShardRouter, error) {
	if len(shards) == 0 {
		return nil, errors.New("at least one shard cluster is required")
	}
	for i, s := range shards {
		if s == nil {
			return nil, fmt.Errorf("shard at index %d cannot be nil", i)
		}
	}
	return &ConsistentHashShardRouter{shards: shards}, nil
}

func (r *ConsistentHashShardRouter) hashPropertyID(propertyID uuid.UUID) uint64 {
	h := sha256.Sum256(propertyID[:])
	return binary.BigEndian.Uint64(h[:8])
}

func (r *ConsistentHashShardRouter) GetShard(propertyID uuid.UUID) *DBCluster {
	val := r.hashPropertyID(propertyID)
	idx := val % uint64(len(r.shards))
	return r.shards[idx]
}

func (r *ConsistentHashShardRouter) Default() *DBCluster {
	return r.shards[0]
}

func (r *ConsistentHashShardRouter) AllShards() []*DBCluster {
	return r.shards
}

func (r *ConsistentHashShardRouter) Close() {
	for _, s := range r.shards {
		if s != nil {
			s.Close()
		}
	}
}

// MapShardRouter routes designated properties to specific clusters, falling back to a default cluster.
type MapShardRouter struct {
	mu          sync.RWMutex
	fallback    *DBCluster
	propertyMap map[uuid.UUID]*DBCluster
	allShards   []*DBCluster
}

// NewMapShardRouter creates a MapShardRouter with a default fallback cluster.
func NewMapShardRouter(fallback *DBCluster) *MapShardRouter {
	return &MapShardRouter{
		fallback:    fallback,
		propertyMap: make(map[uuid.UUID]*DBCluster),
		allShards:   []*DBCluster{fallback},
	}
}

// AssignShard binds a propertyID to a designated DBCluster.
func (r *MapShardRouter) AssignShard(propertyID uuid.UUID, cluster *DBCluster) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.propertyMap[propertyID] = cluster

	// Track unique clusters in allShards
	found := false
	for _, s := range r.allShards {
		if s == cluster {
			found = true
			break
		}
	}
	if !found && cluster != nil {
		r.allShards = append(r.allShards, cluster)
	}
}

func (r *MapShardRouter) GetShard(propertyID uuid.UUID) *DBCluster {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if cluster, exists := r.propertyMap[propertyID]; exists && cluster != nil {
		return cluster
	}
	return r.fallback
}

func (r *MapShardRouter) Default() *DBCluster {
	return r.fallback
}

func (r *MapShardRouter) AllShards() []*DBCluster {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.allShards
}

func (r *MapShardRouter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.allShards {
		if s != nil {
			s.Close()
		}
	}
}
