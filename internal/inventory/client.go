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

// Package inventory provides implementations of inventory clients
package inventory

import (
	"context"
)

// Config is a struct that holds info needed to create a new client implementation
type Config struct {
	Name                string         `yaml:"name"`
	Type                string         `yaml:"type"`
	Options             map[string]any `yaml:"options"`
	HostManagementClass string         `yaml:"hostManagementClass"`
	NetworkClass        string         `yaml:"networkClass"`
}

// Host is the common return type all clients must use
type Host struct {
	BareMetalPoolID     string
	BareMetalPoolHostID string
	InventoryHostID     string
	Name                string
	HostType            string
	HostClass           string
	NetworkClass        string
	ProvisionState      string
	ManagedBy           string
}

// Client interface for inventory implementations
type Client interface {
	// FindFreeHost returns a host with matching fields that is not already assigned
	FindFreeHost(ctx context.Context, matchExpressions map[string]string) (*Host, error)

	// AssignHost updates the host by marking it as assigned, returns the host if the update request was performed
	AssignHost(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*Host, error)

	// UnassignHost updates the host by undoing the assign operation
	UnassignHost(ctx context.Context, inventoryHostID string, labels []string) error
}

// NewClientFunc is a function that creates a new inventory client from config
type NewClientFunc func(ctx context.Context, cfg *Config) (Client, error)

// newClientFuncs is a registry of available inventory client implementations
var newClientFuncs = make(map[string]NewClientFunc)

// NewClient creates a new inventory client based on the config type
func NewClient(ctx context.Context, cfg *Config) (Client, error) {
	newClientFunc, ok := newClientFuncs[cfg.Type]
	if !ok {
		return nil, nil
	}

	return newClientFunc(ctx, cfg)
}
