/*
Copyright 2024 The Kubernetes Authors.

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

package operator

import (
	"context"
	"log"

	"github.com/samber/lo"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	corecluster "sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/apis"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/apis/v1alpha1"
	clusterapi "sigs.k8s.io/karpenter-provider-cluster-api/pkg/cloudprovider"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/operator/options"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers/machine"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers/machinedeployment"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers/cluster"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator"
)

func init() {
	karpv1.RestrictedLabelDomains = karpv1.RestrictedLabelDomains.Insert(v1alpha1.Group)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Insert(
		clusterapi.InstanceSizeLabelKey,
		clusterapi.InstanceFamilyLabelKey,
		clusterapi.InstanceCPULabelKey,
		clusterapi.InstanceMemoryLabelKey,
	)
	lo.Must0(apis.AddToScheme(scheme.Scheme))
}

type Operator struct {
	*operator.Operator

	MachineProvider           machine.Provider
	MachineDeploymentProvider machinedeployment.Provider
	ClusterProvider           cluster.Provider
}

func NewOperator(ctx context.Context, operator *operator.Operator) (context.Context, *Operator) {
	mgmtCluster, err := buildManagementClusterKubeClient(ctx, operator)
	if err != nil {
		log.Fatalf("unable to build management cluster client: %v", err)
	}

	machineProvider := machine.NewDefaultProvider(ctx, mgmtCluster)
	machineDeploymentProvider := machinedeployment.NewDefaultProvider(ctx, mgmtCluster)
	clusterProvider := cluster.NewDefaultProvider(ctx, mgmtCluster)

	return ctx, &Operator{
		Operator:                  operator,
		MachineProvider:           machineProvider,
		MachineDeploymentProvider: machineDeploymentProvider,
		ClusterProvider:           clusterProvider,
	}
}

func buildManagementClusterKubeClient(ctx context.Context, operator *operator.Operator) (client.Client, error) {
	if options.FromContext(ctx).ClusterAPIKubeConfigFile != "" {
		clusterAPIKubeConfig, err := clientcmd.BuildConfigFromFlags("", options.FromContext(ctx).ClusterAPIKubeConfigFile)
		if err != nil {
			return nil, err
		}
		mgmtCluster, err := corecluster.New(clusterAPIKubeConfig, func(o *corecluster.Options) {
			o.Scheme = operator.GetScheme()
		})
		if err != nil {
			return nil, err
		}
		if err = operator.Add(mgmtCluster); err != nil {
			return nil, err
		}
		return mgmtCluster.GetClient(), nil
	}
	return operator.GetClient(), nil
}
