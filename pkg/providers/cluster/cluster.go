/*
Copyright 2025 The Kubernetes Authors.

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

package cluster

import (
	"context"
	"fmt"

	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Provider interface {
	Get(context.Context, string, string) (*capiv1beta1.Cluster, error)
	Update(context.Context, *capiv1beta1.Cluster) error
}

type DefaultProvider struct {
	kubeClient client.Client
}

func NewDefaultProvider(_ context.Context, kubeClient client.Client) *DefaultProvider {
	return &DefaultProvider{
		kubeClient: kubeClient,
	}
}

func (p *DefaultProvider) Get(ctx context.Context, name string, namespace string) (*capiv1beta1.Cluster, error) {
	cluster := &capiv1beta1.Cluster{}
	err := p.kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, cluster)
	if err != nil {
		cluster = nil
	}
	return cluster, err
}

func (p *DefaultProvider) Update(ctx context.Context, cluster *capiv1beta1.Cluster) error {
	err := p.kubeClient.Update(ctx, cluster)
	if err != nil {
		return fmt.Errorf("unable to update Cluster %q: %w", cluster.Name, err)
	}

	return nil
}
