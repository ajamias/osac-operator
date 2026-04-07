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

package inventory

import (
	"context"
	"encoding/json"
	"math/rand"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
)

var (
	_ Client        = (*OpenStackClient)(nil)
	_ NewClientFunc = NewClientFunc(NewOpenStackClient)
)

const (
	OpenStackBareMetalPoolIDKey     = "poolId"
	OpenStackBareMetalPoolHostIDKey = "hostId"
)

func init() {
	newClientFuncs["openstack"] = NewOpenStackClient
}

type OpenStackClient struct {
	client *gophercloud.ServiceClient
}

// NewOpenStackClient creates a new OpenStack inventory client
func NewOpenStackClient(ctx context.Context, cfg *Config) (Client, error) {
	opts := cfg.Options

	var cloud clientconfig.Cloud
	if openstackOpts, ok := opts["openstack"]; ok {
		openstackOptsJSON, err := json.Marshal(openstackOpts)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(openstackOptsJSON, &cloud); err != nil {
			return nil, err
		}
	}

	clientOpts := clientconfig.ClientOpts{
		Cloud:        cloud.Cloud,
		AuthType:     cloud.AuthType,
		AuthInfo:     cloud.AuthInfo,
		RegionName:   cloud.RegionName,
		EndpointType: cloud.EndpointType,
	}

	providerClient, err := clientconfig.AuthenticatedClient(ctx, &clientOpts)
	if err != nil {
		return nil, err
	}

	ironicClient, err := openstack.NewBareMetalV1(providerClient, gophercloud.EndpointOpts{})
	if err != nil {
		return nil, err
	}

	ironicClient.Microversion = "1.72"

	return &OpenStackClient{client: ironicClient}, nil
}

func (c *OpenStackClient) FindFreeHost(ctx context.Context, matchExpressions map[string]string) (*Host, error) {
	listOpts := nodes.ListOpts{
		Fields: []string{
			"uuid",
			"name",
			"resource_class",
			"provision_state",
			"extra",
		},
	}

	if hostType, ok := matchExpressions["hostType"]; ok {
		listOpts.ResourceClass = hostType
	}
	if provisionState, ok := matchExpressions["provisionState"]; ok {
		listOpts.ProvisionState = nodes.ProvisionState(provisionState)
	}

	var foundHost *Host
	err := nodes.List(c.client, listOpts).EachPage(ctx, func(ctx context.Context, page pagination.Page) (bool, error) {
		nodeList, err := nodes.ExtractNodes(page)
		if err != nil {
			return false, err
		}

		// shuffle to reduce chances of getting an unmarked but locked host
		nodes := make([]*nodes.Node, len(nodeList))
		for i := range nodeList {
			nodes[i] = &nodeList[i]
		}
		rand.Shuffle(len(nodes), func(i int, j int) {
			nodes[i], nodes[j] = nodes[j], nodes[i]
		})

		for _, node := range nodes {
			if !c.isAvailableHost(node) {
				continue
			}

			managedBy, ok := node.Extra["managedBy"].(string)
			if !ok {
				managedBy = ""
			}
			if managedBy != matchExpressions["managedBy"] {
				continue
			}

			poolID, ok := node.Extra[OpenStackBareMetalPoolIDKey].(string)
			if !ok {
				poolID = ""
			}

			hostID, ok := node.Extra[OpenStackBareMetalPoolHostIDKey].(string)
			if !ok {
				hostID = ""
			}

			foundHost = &Host{
				BareMetalPoolID:     poolID,
				BareMetalPoolHostID: hostID,
				InventoryHostID:     node.UUID,
				Name:                node.Name,
				HostType:            node.ResourceClass,
				HostClass:           "openstack",
				NetworkClass:        "openstack",
				ProvisionState:      node.ProvisionState,
				ManagedBy:           managedBy,
			}
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return foundHost, nil
}

func (c *OpenStackClient) isAvailableHost(node *nodes.Node) bool {
	hostID, ok := node.Extra[OpenStackBareMetalPoolHostIDKey].(string)
	if !ok {
		hostID = ""
	}
	poolID, ok := node.Extra[OpenStackBareMetalPoolIDKey].(string)
	if !ok {
		poolID = ""
	}
	return hostID == "" && poolID == ""
}

func (c *OpenStackClient) AssignHost(ctx context.Context, inventoryHostID string, poolID string, hostID string, labels map[string]string) (*Host, error) {
	node, err := nodes.Get(ctx, c.client, inventoryHostID).Extract()
	if err != nil {
		return nil, err
	}

	currentBareMetalPoolID, ok := node.Extra[OpenStackBareMetalPoolIDKey].(string)
	if ok && currentBareMetalPoolID != "" && currentBareMetalPoolID != poolID {
		return nil, nil
	}

	currentBareMetalPoolHostID, ok := node.Extra[OpenStackBareMetalPoolHostIDKey].(string)
	if ok && currentBareMetalPoolHostID != "" && currentBareMetalPoolHostID != hostID {
		return nil, nil
	}

	updateOpts := nodes.UpdateOpts{
		nodes.UpdateOperation{
			Op:    nodes.ReplaceOp,
			Path:  "/extra/" + OpenStackBareMetalPoolIDKey,
			Value: poolID,
		},
		nodes.UpdateOperation{
			Op:    nodes.ReplaceOp,
			Path:  "/extra/" + OpenStackBareMetalPoolHostIDKey,
			Value: hostID,
		},
	}
	if labels != nil {
		updateOpts = append(updateOpts, nodes.UpdateOperation{
			Op:    nodes.ReplaceOp,
			Path:  "/extra/labels",
			Value: labels,
		})
	}

	node, err = nodes.Update(ctx, c.client, inventoryHostID, updateOpts).Extract()
	if err != nil {
		return nil, err
	}

	managedBy, ok := node.Extra["managedBy"].(string)
	if !ok {
		managedBy = ""
	}

	return &Host{
		BareMetalPoolID:     poolID,
		BareMetalPoolHostID: hostID,
		InventoryHostID:     node.UUID,
		Name:                node.Name,
		HostType:            node.ResourceClass,
		HostClass:           "openstack",
		NetworkClass:        "openstack",
		ProvisionState:      node.ProvisionState,
		ManagedBy:           managedBy,
	}, nil
}

func (c *OpenStackClient) UnassignHost(ctx context.Context, inventoryHostID string, labels []string) error {
	updateOpts := nodes.UpdateOpts{
		nodes.UpdateOperation{
			Op:    nodes.ReplaceOp,
			Path:  "/extra/" + OpenStackBareMetalPoolIDKey,
			Value: "",
		},
		nodes.UpdateOperation{
			Op:    nodes.ReplaceOp,
			Path:  "/extra/" + OpenStackBareMetalPoolHostIDKey,
			Value: "",
		},
	}
	for _, label := range labels {
		updateOpts = append(updateOpts, nodes.UpdateOperation{
			Op:   nodes.RemoveOp,
			Path: "/extra/labels/" + label,
		})
	}

	_, err := nodes.Update(ctx, c.client, inventoryHostID, updateOpts).Extract()
	return err
}
