package postgres

import (
	"testing"

	"github.com/google/uuid"
)

func TestSingleShardRouter(t *testing.T) {
	cluster := &DBCluster{}
	router := NewSingleShardRouter(cluster)

	id := uuid.New()
	if got := router.GetShard(id); got != cluster {
		t.Fatalf("expected router to return wrapped cluster")
	}
	if got := router.Default(); got != cluster {
		t.Fatalf("expected Default() to return wrapped cluster")
	}
	if shards := router.AllShards(); len(shards) != 1 || shards[0] != cluster {
		t.Fatalf("expected AllShards() to contain exactly the single cluster")
	}
}

func TestConsistentHashShardRouter_Distribution(t *testing.T) {
	s1 := &DBCluster{}
	s2 := &DBCluster{}
	s3 := &DBCluster{}

	router, err := NewConsistentHashShardRouter(s1, s2, s3)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if router.Default() != s1 {
		t.Fatalf("expected Default() to be s1")
	}

	counts := make(map[*DBCluster]int)
	// Sample 300 random UUIDs and check distribution across shards
	for i := 0; i < 300; i++ {
		id := uuid.New()
		shard := router.GetShard(id)
		counts[shard]++

		// Assert deterministic routing for the same ID
		if router.GetShard(id) != shard {
			t.Fatalf("expected deterministic routing for same property ID")
		}
	}

	// Verify all 3 shards received traffic
	if counts[s1] == 0 || counts[s2] == 0 || counts[s3] == 0 {
		t.Fatalf("expected all shards to receive property traffic, got %v", counts)
	}
}

func TestConsistentHashShardRouter_Validation(t *testing.T) {
	if _, err := NewConsistentHashShardRouter(); err == nil {
		t.Fatalf("expected error on empty shards list")
	}
	if _, err := NewConsistentHashShardRouter(nil); err == nil {
		t.Fatalf("expected error on nil shard")
	}
}

func TestMapShardRouter(t *testing.T) {
	defaultCluster := &DBCluster{}
	specialCluster := &DBCluster{}

	router := NewMapShardRouter(defaultCluster)

	p1 := uuid.New()
	p2 := uuid.New()

	// Initially all route to default
	if got := router.GetShard(p1); got != defaultCluster {
		t.Fatalf("expected default cluster before assignment")
	}

	// Assign p2 to special cluster
	router.AssignShard(p2, specialCluster)

	if got := router.GetShard(p2); got != specialCluster {
		t.Fatalf("expected special cluster for assigned property")
	}
	if got := router.GetShard(p1); got != defaultCluster {
		t.Fatalf("expected default cluster for unassigned property")
	}

	all := router.AllShards()
	if len(all) != 2 {
		t.Fatalf("expected 2 unique shards in AllShards(), got %d", len(all))
	}
}
