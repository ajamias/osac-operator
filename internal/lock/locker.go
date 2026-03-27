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

// Package lock provides implementations of distributed locking mechanisms
package lock

import (
	"context"
	"time"
)

// Config is a struct that holds info needed to create a new locker implementation
type Config struct {
	Type    string         `yaml:"type"`
	TTL     time.Duration  `yaml:"ttl"`
	Options map[string]any `yaml:"options"`
}

// Locker defines the interface for distributed locking
type Locker interface {
	// TryLock attempts to acquire a lock for the given key
	// Returns true if lock was acquired, false if already locked
	TryLock(ctx context.Context, key string) (bool, error)

	// Unlock releases the lock for the given key
	Unlock(ctx context.Context, key string) error

	// ExtendLock extends the TTL of an existing lock
	ExtendLock(ctx context.Context, key string) error
}

// NewLockerFunc is a function that creates a new locker from config
type NewLockerFunc func(ctx context.Context, cfg *Config) (Locker, error)

// newLockerFuncs is a registry of available locker implementations
var newLockerFuncs = make(map[string]NewLockerFunc)

// NewLocker creates a new locker based on the config type
func NewLocker(ctx context.Context, cfg *Config) (Locker, error) {
	newLockerFunc, ok := newLockerFuncs[cfg.Type]
	if !ok {
		return nil, nil
	}

	return newLockerFunc(ctx, cfg)
}
