// This implementation is based on and modified from https://github.com/fanjindong/go-cache
package cache

import (
	"fmt"

	"github.com/cespare/xxhash/v2"
)

func keyToString[K comparable](key K) string {
	return fmt.Sprintf("%v", key)
}

type Cache[K comparable, V any] interface {
	Set(k K, v V)
	Get(k K) (V, bool)
	GetAll() map[K]V
	ReplaceAll(items map[K]V)
	Update(k K, fn func(current V, exists bool) (V, bool)) (V, bool)
	Del(keys ...K) int
	Exists(keys ...K) bool
	Len() int
	Clear()
}

func New[K comparable, V any](shards int) Cache[K, V] {
	if shards <= 0 {
		shards = 1024
	}

	c := &cache[K, V]{
		shards: make([]*shard[K, V], shards),
	}
	for i := 0; i < shards; i++ {
		c.shards[i] = &shard[K, V]{hashmap: map[K]V{}}
	}

	return c
}

type cache[K comparable, V any] struct {
	shards []*shard[K, V]
}

func (c *cache[K, V]) Set(k K, v V) {
	hashedKey := xxhash.Sum64String(keyToString(k))
	shard := c.getShard(hashedKey)
	shard.set(k, v)
}

func (c *cache[K, V]) Get(k K) (V, bool) {
	hashedKey := xxhash.Sum64String(keyToString(k))
	shard := c.getShard(hashedKey)
	return shard.get(k)
}

func (c *cache[K, V]) GetAll() map[K]V {
	result := make(map[K]V)
	for _, shard := range c.shards {
		shardData := shard.getAll()
		for k, v := range shardData {
			result[k] = v
		}
	}
	return result
}

// ReplaceAll swaps the whole cache with a prepared snapshot.
func (c *cache[K, V]) ReplaceAll(items map[K]V) {
	shardMaps := make([]map[K]V, len(c.shards))
	for k, v := range items {
		index := c.getShardIndex(xxhash.Sum64String(keyToString(k)))
		if shardMaps[index] == nil {
			shardMaps[index] = make(map[K]V)
		}
		shardMaps[index][k] = v
	}

	for i, s := range c.shards {
		if shardMaps[i] == nil {
			shardMaps[i] = map[K]V{}
		}
		s.replace(shardMaps[i])
	}
}

// Update runs a read-modify-write operation while holding the target shard lock.
func (c *cache[K, V]) Update(k K, fn func(current V, exists bool) (V, bool)) (V, bool) {
	hashedKey := xxhash.Sum64String(keyToString(k))
	shard := c.getShard(hashedKey)
	return shard.update(k, fn)
}

func (c *cache[K, V]) Del(ks ...K) int {
	var count int
	for _, k := range ks {
		hashedKey := xxhash.Sum64String(keyToString(k))
		shard := c.getShard(hashedKey)
		count += shard.del(k)
	}
	return count
}

func (c *cache[K, V]) Exists(ks ...K) bool {
	for _, k := range ks {
		if _, found := c.Get(k); !found {
			return false
		}
	}
	return true
}

func (c *cache[K, V]) Len() int {
	var count int
	for _, shard := range c.shards {
		count += shard.len()
	}
	return count
}

func (c *cache[K, V]) getShard(hashedKey uint64) (shard *shard[K, V]) {
	return c.shards[c.getShardIndex(hashedKey)]
}

func (c *cache[K, V]) getShardIndex(hashedKey uint64) int {
	return int(hashedKey % uint64(len(c.shards)))
}

func (c *cache[K, V]) Clear() {
	for _, s := range c.shards {
		s.clear()
	}
}
