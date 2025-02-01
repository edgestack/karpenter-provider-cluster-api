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

package controllers

import (
	"context"

	"github.com/awslabs/operatorpkg/controller"
	"github.com/awslabs/operatorpkg/status"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/apis/v1alpha1"
	nodeclasshash "sigs.k8s.io/karpenter-provider-cluster-api/pkg/controllers/nodeclass/hash"
	nodeclassstatus "sigs.k8s.io/karpenter-provider-cluster-api/pkg/controllers/nodeclass/status"
)

func NewControllers(ctx context.Context, mgr manager.Manager, kubeClient client.Client, recorder events.Recorder,
	cloudProvider cloudprovider.CloudProvider) []controller.Controller {
	controllers := []controller.Controller{
		nodeclasshash.NewController(kubeClient),
		nodeclassstatus.NewController(kubeClient),

		// TODO: nodeclaim tagging
		status.NewController[*v1alpha1.ClusterAPINodeClass](kubeClient, mgr.GetEventRecorderFor("karpenter")),
	}
	return controllers
}
