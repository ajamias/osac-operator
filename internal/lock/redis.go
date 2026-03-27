/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lock

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	_ Locker        = (*redisLocker)(nil)
	_ NewLockerFunc = NewLockerFunc(NewRedisLocker)
)

func init() {
	newLockerFuncs["redis"] = NewRedisLocker
}

// redisLocker provides distributed locking using Redis
type redisLocker struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedisLocker creates a new Redis-based distributed locker
func NewRedisLocker(ctx context.Context, cfg *Config) (Locker, error) {
	opts := cfg.Options

	addr, ok := opts["addr"].(string)
	if !ok {
		addr = "localhost:6379"
	}

	password, _ := opts["password"].(string)

	db := 0
	if dbVal, ok := opts["db"].(int); ok {
		db = dbVal
	}

	redisClient := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	// Test Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis at %s: %w", addr, err)
	}

	ttl := cfg.TTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}

	return &redisLocker{
		client: redisClient,
		ttl:    ttl,
	}, nil
}

// TryLock attempts to acquire a lock for the given key
// Returns true if lock was acquired, false if already locked
func (l *redisLocker) TryLock(ctx context.Context, key string) (bool, error) {
	lockKey := fmt.Sprintf("node-lock:%s", key)

	// Use SET with NX (set if not exists) and expiration
	// This is atomic and prevents race conditions
	result, err := l.client.SetArgs(ctx, lockKey, "locked", redis.SetArgs{
		Mode: "NX",
		TTL:  l.ttl,
	}).Result()
	if err != nil {
		// redis.Nil means the key already exists (lock not acquired)
		if err == redis.Nil {
			return false, nil
		}
		return false, fmt.Errorf("failed to acquire lock for key %s: %w", key, err)
	}

	// SetArgs with NX returns "OK" if the key was set, empty string if it already exists
	return result == "OK", nil
}

// Unlock releases the lock for the given key
func (l *redisLocker) Unlock(ctx context.Context, key string) error {
	lockKey := fmt.Sprintf("node-lock:%s", key)

	err := l.client.Del(ctx, lockKey).Err()
	if err != nil {
		return fmt.Errorf("failed to release lock for key %s: %w", key, err)
	}

	return nil
}

// ExtendLock extends the TTL of an existing lock
func (l *redisLocker) ExtendLock(ctx context.Context, key string) error {
	lockKey := fmt.Sprintf("node-lock:%s", key)

	result, err := l.client.Expire(ctx, lockKey, l.ttl).Result()
	if err != nil {
		return fmt.Errorf("failed to extend lock for key %s: %w", key, err)
	}

	if !result {
		return fmt.Errorf("lock for key %s does not exist", key)
	}

	return nil
}
