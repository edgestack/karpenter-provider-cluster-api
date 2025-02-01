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

package cloudprovider

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"
	"sort"

	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers/cluster"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers/machine"
	"sigs.k8s.io/karpenter-provider-cluster-api/pkg/providers/machinedeployment"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	// TODO (elmiko) if we make these exposed constants from the CAS we can import them instead of redifining and risking drift
	cpuKey          = "capacity.cluster-autoscaler.kubernetes.io/cpu"
	memoryKey       = "capacity.cluster-autoscaler.kubernetes.io/memory"
	gpuCountKey     = "capacity.cluster-autoscaler.kubernetes.io/gpu-count"
	gpuTypeKey      = "capacity.cluster-autoscaler.kubernetes.io/gpu-type"
	diskCapacityKey = "capacity.cluster-autoscaler.kubernetes.io/ephemeral-disk"
	labelsKey       = "capacity.cluster-autoscaler.kubernetes.io/labels"
	taintsKey       = "capacity.cluster-autoscaler.kubernetes.io/taints"
	maxPodsKey      = "capacity.cluster-autoscaler.kubernetes.io/maxPods"
)

func NewCloudProvider(ctx context.Context, kubeClient client.Client, machineProvider machine.Provider, machineDeploymentProvider machinedeployment.Provider, clusterProvider cluster.Provider) *CloudProvider {
	return &CloudProvider{
		kubeClient:                kubeClient,
		machineProvider:           machineProvider,
		machineDeploymentProvider: machineDeploymentProvider,
		clusterProvider:           clusterProvider,
	}
}

type ClusterAPIInstanceType struct {
	cloudprovider.InstanceType

	MachineDeploymentName      string
	MachineDeploymentNamespace string
}

type CloudProvider struct {
	kubeClient                client.Client
	accessLock                sync.Mutex
	clusterProvider           cluster.Provider
	machineProvider           machine.Provider
	machineDeploymentProvider machinedeployment.Provider
}

func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	// to eliminate racing if multiple creation occur, we gate access to this function
	c.accessLock.Lock()
	defer c.accessLock.Unlock()

	if nodeClaim == nil {
		return nil, fmt.Errorf("cannot satisfy create, NodeClaim is nil")
	}

	nodeClass, err := c.resolveNodeClassFromNodeClaim(ctx, nodeClaim)
	if err != nil {
		return nil, fmt.Errorf("cannot satisfy create, unable to resolve NodeClass from NodeClaim %q: %w", nodeClaim.Name, err)
	}

	instanceTypes, err := c.findInstanceTypesForNodeClass(ctx, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("cannot satisfy create, unable to get instance types for NodeClass %q of NodeClaim %q: %w", nodeClass.Name, nodeClaim.Name, err)
	}

	// identify which fit requirements
	compatibleInstanceTypes := filterCompatibleInstanceTypes(instanceTypes, nodeClaim)
	if len(compatibleInstanceTypes) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("cannot satisfy create, no compatible instance types found"))
	}

	// sort the instance type wrt the resource type in ascending order
	sortedInstanceTypes := sortInstanceTypes(compatibleInstanceTypes)
	selectedInstanceType := sortedInstanceTypes[0]

	// once scalable resource is identified, increase replicas
	machineDeployment, err := c.machineDeploymentProvider.Get(ctx, selectedInstanceType.MachineDeploymentName, selectedInstanceType.MachineDeploymentNamespace)
	if err != nil {
		return nil, fmt.Errorf("cannot satisfy create, unable to find MachineDeployment %q for InstanceType %q: %w", selectedInstanceType.MachineDeploymentName, selectedInstanceType.Name, err)
	}
	originalReplicas := *machineDeployment.Spec.Replicas

	clusterName := machineDeployment.Spec.ClusterName
	clusterNamespace := machineDeployment.Namespace

	// Extract ClusterClassName by removing the known clusterName prefix
	clusterClassName := strings.TrimPrefix(machineDeployment.Name, clusterName + "-")
	newReplicaCount := ptr.To(originalReplicas + 1)
	cluster, err := c.clusterProvider.Get(ctx, clusterName, clusterNamespace)

	if err != nil {
                return nil, fmt.Errorf("cannot satisfy create, unable to find Cluster %q: %w", clusterName, err)
	}

	// Find the corresponding machineDeployment in Cluster topology
	updated := false
	for i, md := range cluster.Spec.Topology.Workers.MachineDeployments {
		if md.Name == clusterClassName {
			fmt.Printf("Found matching MachineDeployment: %s, updating replicas to %d\n", md.Name, newReplicaCount)
			cluster.Spec.Topology.Workers.MachineDeployments[i].Replicas = newReplicaCount
			updated = true
			break
		}
	}

	if !updated {
		return nil, fmt.Errorf("MachineDeployment %s not found in Cluster %s", clusterClassName, clusterName)
	} else {
		if err := c.clusterProvider.Update(ctx, cluster); err != nil {
			return nil, fmt.Errorf("cannot satisfy create, unable to update MachineDeployment Class %q on Cluster %q with replicas: %w", clusterClassName, clusterName, err)
		}
	}

	machineDeployment.Spec.Replicas = newReplicaCount

	// Retry updating the Machine to avoid conflicts
	retryInterval := 200 * time.Millisecond
	maxRetries := 10
	globalMachineName := ""

	for i := 0; i < maxRetries; i++ {
		// Fetch the latest Machine object to avoid stale resourceVersion
		machine, err := c.pollForUnclaimedMachineInMachineDeploymentWithTimeout(ctx, machineDeployment, time.Minute)
		if err != nil {
			fmt.Printf("failed to re-fetch Machine %q: %w", machine.Name, err)
			continue
		} else {
			globalMachineName = machine.Name
		}

		// Update labels
		labels := machine.GetLabels()
		labels[providers.NodePoolMemberLabel] = ""
		machine.SetLabels(labels)

		// Attempt to update the Machine
		err = c.machineProvider.Update(ctx, machine)
		if err == nil {
			break // Update successful
		}

		if err != nil {
			fmt.Printf("unable to label Machine %q as a member: %w, no worry, we will retry...", machine.Name, err)
		}

		// If conflict, wait and retry with exponential backoff
		time.Sleep(retryInterval)
		retryInterval *= 2 // Exponential backoff
	}

	//  fill out nodeclaim with details
        createdNodeClaim := createNodeClaimFromMachineDeployment(machineDeployment)

        // (FIXME) we hard-code the provider ID for kubevirt for now
        createdNodeClaim.Status.ProviderID = "kubevirt://" + globalMachineName

	return createdNodeClaim, nil
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	// to eliminate racing if multiple deletion occur, we gate access to this function
	c.accessLock.Lock()
	defer c.accessLock.Unlock()

	if len(nodeClaim.Status.ProviderID) == 0 {
		return fmt.Errorf("NodeClaim %q does not have a provider ID, cannot delete", nodeClaim.Name)
	}

	// find machine
	machine, err := c.machineProvider.Get(ctx, nodeClaim.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("error finding Machine with provider ID %q to Delete NodeClaim %q: %w", nodeClaim.Status.ProviderID, nodeClaim.Name, err)
	}
	if machine == nil {
		return fmt.Errorf("unable to find Machine with provider ID %q to Delete NodeClaim %q", nodeClaim.Status.ProviderID, nodeClaim.Name)
	}

	// check if already deleting
	if c.machineProvider.IsDeleting(machine) {
		// Machine is already deleting, we do not need to annotate it or change the scalable resource replicas.
		return nil
	}

	// check if reducing replicas goes below zero
	machineDeployment, err := c.machineDeploymentFromMachine(ctx, machine)
	if err != nil {
		return fmt.Errorf("unable to delete NodeClaim %q, cannot find an owner MachineDeployment for Machine %q: %w", nodeClaim.Name, machine.Name, err)
	}

	if machineDeployment.Spec.Replicas == nil {
		return fmt.Errorf("unable to delete NodeClaim %q, MachineDeployment %q has nil replicas", nodeClaim.Name, machineDeployment.Name)
	}

	if *machineDeployment.Spec.Replicas == 0 {
		return fmt.Errorf("unable to delete NodeClaim %q, MachineDeployment %q is already at zero replicas", nodeClaim.Name, machineDeployment.Name)
	}

	// mark the machine for deletion before decrementing replicas to protect against the wrong machine being removed
	err = c.machineProvider.AddDeleteAnnotation(ctx, machine)
	if err != nil {
		return fmt.Errorf("unable to delete NodeClaim %q, cannot annotate Machine %q for deletion: %w", nodeClaim.Name, machine.Name, err)
	}

	//   and reduce machinedeployment replicas
	updatedReplicas := *machineDeployment.Spec.Replicas - 1
	machineDeployment.Spec.Replicas = ptr.To(updatedReplicas)

	clusterName := machineDeployment.Spec.ClusterName
        clusterNamespace := machineDeployment.Namespace

	// Extract ClusterClassName by removing the known clusterName prefix
        clusterClassName := strings.TrimPrefix(machineDeployment.Name, clusterName + "-")
        cluster, err := c.clusterProvider.Get(ctx, clusterName, clusterNamespace)

        if err != nil {
                return fmt.Errorf("unable to delete NodeClaim %q, unable to find Cluster %q: %w", nodeClaim.Name, clusterName, err)
        }

        // Find the corresponding machineDeployment in Cluster topology
        updated := false
        for i, md := range cluster.Spec.Topology.Workers.MachineDeployments {
                if md.Name == clusterClassName {
                        fmt.Printf("Found matching MachineDeployment: %s, updating replicas to %d\n", md.Name, ptr.To(updatedReplicas))
                        cluster.Spec.Topology.Workers.MachineDeployments[i].Replicas = ptr.To(updatedReplicas)
                        updated = true
                        break
                }
        }

        if !updated {
		// cleanup the machine delete annotation so we don't affect future replica changes
                if err := c.machineProvider.RemoveDeleteAnnotation(ctx, machine); err != nil {
                        return fmt.Errorf("unable to delete NodeClaim %q, cannot remove delete annotation for Machine %q during cleanup: %w", nodeClaim.Name, machine.Name, err)
                }

                return fmt.Errorf("MachineDeployment %s not found in Cluster %s", clusterClassName, clusterName)
        } else {
                if err := c.clusterProvider.Update(ctx, cluster); err != nil {
                        return fmt.Errorf("unable to delete NodeClaim %q, unable to update MachineDeployment Class %q on Cluster %q with replicas: %w", nodeClaim.Name, clusterClassName, clusterName, err)
                }
        }

	return nil
}

// Get returns a NodeClaim for the Machine object with the supplied provider ID, or nil if not found.
func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	if len(providerID) == 0 {
		return nil, fmt.Errorf("no providerID supplied to Get, cannot continue")
	}

	machine, err := c.machineProvider.Get(ctx, providerID)
	if err != nil {
		return nil, fmt.Errorf("unable to find Machine for Get: %w", err)
	}
	if machine == nil {
		return nil, nil
	}

	nodeClaim, err := c.machineToNodeClaim(ctx, machine)
	if err != nil {
		return nil, fmt.Errorf("unable to convert Machine to NodeClaim in CloudProvider.Get: %w", err)
	}

	return nodeClaim, nil
}

// GetInstanceTypes enumerates the known Cluster API scalable resources to generate the list
// of possible instance types.
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {

	if nodePool == nil {
		return nil, fmt.Errorf("node pool reference is nil, no way to proceed")
	}

	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		return nil, err
	}

	capiInstanceTypes, err := c.findInstanceTypesForNodeClass(ctx, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("unable to get instance types for NodePool %q: %w", nodePool.Name, err)
	}

	instanceTypes := lo.Map(capiInstanceTypes, func(i *ClusterAPIInstanceType, _ int) *cloudprovider.InstanceType {
		return &cloudprovider.InstanceType{
			Name:         i.Name,
			Requirements: i.Requirements,
			Offerings:    i.Offerings,
			Capacity:     i.Capacity,
			Overhead:     i.Overhead,
		}
	})
	return instanceTypes, nil
}

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.ClusterAPINodeClass{}}
}

// Return nothing since there's no cloud provider drift.
func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", nil
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	// select all machines that have the nodepool membership label, this should be all the machines that are registered as nodes
	selector := metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      providers.NodePoolMemberLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}
	machines, err := c.machineProvider.List(ctx, &selector)
	if err != nil {
		return nil, fmt.Errorf("listing machines, %w", err)
	}

	var nodeClaims []*karpv1.NodeClaim
	for _, machine := range machines {
		nodeClaim, err := c.machineToNodeClaim(ctx, machine)
		if err != nil {
			return []*karpv1.NodeClaim{}, err
		}
		nodeClaims = append(nodeClaims, nodeClaim)
	}

	return nodeClaims, nil
}

func (c *CloudProvider) Name() string {
	return "clusterapi"
}

func (c *CloudProvider) machineDeploymentFromMachine(ctx context.Context, machine *capiv1beta1.Machine) (*capiv1beta1.MachineDeployment, error) {
	mdName, found := machine.GetLabels()[capiv1beta1.MachineDeploymentNameLabel]
	if !found {
		return nil, fmt.Errorf("unable to find MachineDeployment for Machine %q, has no MachineDeployment label %q", machine.GetName(), capiv1beta1.MachineDeploymentNameLabel)
	}

	machineDeployment, err := c.machineDeploymentProvider.Get(ctx, mdName, machine.GetNamespace())
	if err != nil {
		return nil, err
	}

	return machineDeployment, nil
}

func (c *CloudProvider) findInstanceTypesForNodeClass(ctx context.Context, nodeClass *v1alpha1.ClusterAPINodeClass) ([]*ClusterAPIInstanceType, error) {
	instanceTypes := []*ClusterAPIInstanceType{}

	if nodeClass == nil {
		return instanceTypes, fmt.Errorf("unable to find instance types for nil NodeClass")
	}

	machineDeployments, err := c.machineDeploymentProvider.List(ctx, nodeClass.Spec.ScalableResourceSelector)
	if err != nil {
		return instanceTypes, err
	}

	for _, md := range machineDeployments {
		it := machineDeploymentToInstanceType(md)
		instanceTypes = append(instanceTypes, it)
	}

	return instanceTypes, nil
}

func (c *CloudProvider) machineToNodeClaim(ctx context.Context, machine *capiv1beta1.Machine) (*karpv1.NodeClaim, error) {
	nodeClaim := karpv1.NodeClaim{}
	if machine.Spec.ProviderID != nil {
		nodeClaim.Status.ProviderID = *machine.Spec.ProviderID
	}

	// we want to get the MachineDeployment that owns this Machine to read the capacity information.
	// to being this process, we get the MachineDeployment name from the Machine labels.
	machineDeployment, err := c.machineDeploymentFromMachine(ctx, machine)
	if err != nil {
		return nil, fmt.Errorf("unable to convert Machine %q to a NodeClaim, cannot find MachineDeployment: %w", machine.Name, err)
	}

	// machine capacity
	// we are using the scale from zero annotations on the MachineDeployment to make this accessible.
	// TODO (elmiko) improve this once upstream has advanced the state of the art for getting capacity,
	// also add a mechanism to lookup the infra machine template similar to how CAS does it.
	capacity := capacityResourceListFromAnnotations(machineDeployment.GetAnnotations())
	_, found := capacity[corev1.ResourceCPU]
	if !found {
		// if there is no cpu resource we aren't going to get far, return an error
		return nil, fmt.Errorf("unable to convert Machine %q to a NodeClaim, no cpu capacity found on MachineDeployment %q", machine.GetName(), machineDeployment.Name)
	}

	_, found = capacity[corev1.ResourceMemory]
	if !found {
		// if there is no memory resource we aren't going to get far, return an error
		return nil, fmt.Errorf("unable to convert Machine %q to a NodeClaim, no memory capacity found on MachineDeployment %q", machine.GetName(), machineDeployment.Name)
	}

	// TODO (elmiko) add labels, and taints

	nodeClaim.Status.Capacity = capacity

	return &nodeClaim, nil
}

func (c *CloudProvider) pollForUnclaimedMachineInMachineDeploymentWithTimeout(ctx context.Context, machineDeployment *capiv1beta1.MachineDeployment, timeout time.Duration) (*capiv1beta1.Machine, error) {
	var machine *capiv1beta1.Machine

	// select all Machines that have the ownership label for the MachineDeployment, and do not have the
	// label for karpenter node pool membership.
	selector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      providers.NodePoolMemberLabel,
				Operator: metav1.LabelSelectorOpDoesNotExist,
			},
			{
				Key:      capiv1beta1.MachineDeploymentNameLabel,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{machineDeployment.Name},
			},
		},
	}

	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		machineList, err := c.machineProvider.List(ctx, selector)
		if err != nil {
			// this might need to ignore the error for the sake of the timeout
			return false, fmt.Errorf("error listing unclaimed Machines for MachineDeployment %q: %w", machineDeployment.Name, err)
		}
		if len(machineList) == 0 {
			return false, nil
		}

		// take the first Machine from the list
		machine = machineList[0]

		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("error polling for an unclaimed Machine in MachineDeployment %q: %w", machineDeployment.Name, err)
	}

	return machine, nil
}

func (c *CloudProvider) resolveNodeClassFromNodeClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*v1alpha1.ClusterAPINodeClass, error) {
	nodeClass := &v1alpha1.ClusterAPINodeClass{}

	if nodeClaim == nil {
		return nil, fmt.Errorf("NodeClaim is nil, cannot resolve NodeClass")
	}

	if nodeClaim.Spec.NodeClassRef == nil {
		return nil, fmt.Errorf("NodeClass reference is nil for NodeClaim %q, cannot resolve NodeClass", nodeClaim.Name)
	}

	name := nodeClaim.Spec.NodeClassRef.Name
	if name == "" {
		return nil, fmt.Errorf("NodeClass reference name is empty for NodeClaim %q, cannot resolve NodeClass", nodeClaim.Name)
	}

	// TODO (elmiko) add extra logic to get different resources from the class ref
	// if the kind and version differ from the included api then we will need to load differently.
	// NodeClass and NodeClaim are not namespace scoped, this call can probably be changed.
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: nodeClaim.Namespace}, nodeClass); err != nil {
		return nil, err
	}

	return nodeClass, nil
}

func (c *CloudProvider) resolveNodeClassFromNodePool(ctx context.Context, nodePool *karpv1.NodePool) (*v1alpha1.ClusterAPINodeClass, error) {
	nodeClass := &v1alpha1.ClusterAPINodeClass{}

	if nodePool == nil {
		return nil, fmt.Errorf("NodePool is nil, cannot resolve NodeClass")
	}

	if nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return nil, fmt.Errorf("node class reference is nil, no way to proceed")
	}

	name := nodePool.Spec.Template.Spec.NodeClassRef.Name
	if name == "" {
		return nil, fmt.Errorf("node class reference name is empty, no way to proceed")
	}

	// TODO (elmiko) add extra logic to get different resources from the class ref
	// if the kind and version differ from the included api then we will need to load differently.
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: nodePool.Namespace}, nodeClass); err != nil {
		return nil, err
	}

	return nodeClass, nil
}

func capacityResourceListFromAnnotations(annotations map[string]string) corev1.ResourceList {
	capacity := corev1.ResourceList{}

	if annotations == nil {
		return capacity
	}

	cpu, found := annotations[cpuKey]
	if found {
		capacity[corev1.ResourceCPU] = resource.MustParse(cpu)
	}

	memory, found := annotations[memoryKey]
	if found {
		capacity[corev1.ResourceMemory] = resource.MustParse(memory)
	}

	gpuCount, found := annotations[gpuCountKey]
	if found {
		// if there is a count there must also be a type
		gpuType, found := annotations[gpuTypeKey]
		if found {
			capacity[corev1.ResourceName(gpuType)] = resource.MustParse(gpuCount)
		}
	}

	ephStorage, found := annotations[diskCapacityKey]
	if found {
		capacity[corev1.ResourceEphemeralStorage] = resource.MustParse(ephStorage)
	}

	maxPods, found := annotations[maxPodsKey]
	if found {
		capacity[corev1.ResourcePods] = resource.MustParse(maxPods)
	}

	return capacity
}

func createNodeClaimFromMachineDeployment(machineDeployment *capiv1beta1.MachineDeployment) *karpv1.NodeClaim {
	nodeClaim := &karpv1.NodeClaim{}

	instanceType := machineDeploymentToInstanceType(machineDeployment)
	nodeClaim.Status.Capacity = instanceType.Capacity
	nodeClaim.Status.Allocatable = instanceType.Allocatable()

	// TODO (elmiko) we might need to also convey the labels and annotations on to the NodeClaim

	return nodeClaim
}

func filterCompatibleInstanceTypes(instanceTypes []*ClusterAPIInstanceType, nodeClaim *karpv1.NodeClaim) []*ClusterAPIInstanceType {
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	filteredInstances := lo.Filter(instanceTypes, func(i *ClusterAPIInstanceType, _ int) bool {
		// TODO (elmiko) if/when we have offering availability, this is a good place to filter out unavailable instance types
		return reqs.Compatible(i.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil &&
			resources.Fits(nodeClaim.Spec.Resources.Requests, i.Allocatable())
	})

	return filteredInstances
}

func labelsFromScaleFromZeroAnnotation(annotation string) map[string]string {
	labels := map[string]string{}

	labelStrings := strings.Split(annotation, ",")
	for _, label := range labelStrings {
		split := strings.SplitN(label, "=", 2)
		if len(split) == 2 {
			labels[split[0]] = split[1]
		}
	}

	return labels
}

func machineDeploymentToInstanceType(machineDeployment *capiv1beta1.MachineDeployment) *ClusterAPIInstanceType {
	instanceType := &ClusterAPIInstanceType{}

	labels := nodeLabelsFromMachineDeployment(machineDeployment)
	requirements := []*scheduling.Requirement{}
	for k, v := range labels {
		requirements = append(requirements, scheduling.NewRequirement(k, corev1.NodeSelectorOpIn, v))
	}
	instanceType.Requirements = scheduling.NewRequirements(requirements...)

	capacity := capacityResourceListFromAnnotations(machineDeployment.GetAnnotations())
	instanceType.Capacity = capacity

	// TODO (elmiko) add offerings info, TBD of where this would come from
	// start with zone, read from the label and add to offering
	// initial price is 0
	// there is a single offering, and it is available
	zone := zoneLabelFromLabels(labels)
	requirements = []*scheduling.Requirement{
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
	}
	offerings := cloudprovider.Offerings{
		cloudprovider.Offering{
			Requirements: scheduling.NewRequirements(requirements...),
			Price:        0.0,
			Available:    true,
		},
	}

	instanceType.Offerings = offerings
	// TODO (elmiko) this may not be correct given the code comment in the InstanceType struct about the name corresponding
	// to the v1.LabelInstanceTypeStable. if karpenter expects this to match the node, then we need to get this value through capi.
	instanceType.Name = machineDeployment.Name

	// TODO (elmiko) add the proper overhead information, not sure where we will harvest this information.
	// perhaps it needs to be a configurable option somewhere.
	instanceType.Overhead = &cloudprovider.InstanceTypeOverhead{}

	// record the information from the MachineDeployment so we can find it again later.
	instanceType.MachineDeploymentName = machineDeployment.Name
	instanceType.MachineDeploymentNamespace = machineDeployment.Namespace

	return instanceType
}

func managedNodeLabelsFromLabels(labels map[string]string) map[string]string {
	managedLabels := map[string]string{}
	for key, value := range labels {
		dnsSubdomainOrName := strings.Split(key, "/")[0]
		if dnsSubdomainOrName == capiv1beta1.NodeRoleLabelPrefix {
			managedLabels[key] = value
		}
		if dnsSubdomainOrName == capiv1beta1.NodeRestrictionLabelDomain || strings.HasSuffix(dnsSubdomainOrName, "."+capiv1beta1.NodeRestrictionLabelDomain) {
			managedLabels[key] = value
		}
		if dnsSubdomainOrName == capiv1beta1.ManagedNodeLabelDomain || strings.HasSuffix(dnsSubdomainOrName, "."+capiv1beta1.ManagedNodeLabelDomain) {
			managedLabels[key] = value
		}
	}

	return managedLabels
}

func nodeLabelsFromMachineDeployment(machineDeployment *capiv1beta1.MachineDeployment) map[string]string {
	labels := map[string]string{}

	if machineDeployment.Spec.Template.Labels != nil {
		// get the labels that will be propagated to the node from the machinedeployment
		// see https://cluster-api.sigs.k8s.io/developer/architecture/controllers/metadata-propagation#metadata-propagation
		labels = managedNodeLabelsFromLabels(machineDeployment.Spec.Template.Labels)
	}

	// next we integrate labels from the scale-from-zero annotations, these can override
	// the propagated labels.
	// see https://github.com/kubernetes-sigs/cluster-api/blob/main/docs/proposals/20210310-opt-in-autoscaling-from-zero.md#machineset-and-machinedeployment-annotations
	if annotation, found := machineDeployment.GetAnnotations()[labelsKey]; found {
		for k, v := range labelsFromScaleFromZeroAnnotation(annotation) {
			labels[k] = v
		}
	}

	return labels
}

// zoneLabelFromLabels returns the value of the kubernetes well-known zone label or an empty string
func zoneLabelFromLabels(labels map[string]string) string {
	zone := ""

	if value, found := labels[corev1.LabelTopologyZone]; found {
		zone = value
	}

	return zone
}

func sortInstanceTypes(instanceTypes []*ClusterAPIInstanceType) []*ClusterAPIInstanceType {
	// Create a copy to avoid modifying the original slice
	sortedInstances := make([]*ClusterAPIInstanceType, len(instanceTypes))
	copy(sortedInstances, instanceTypes)

	sort.Slice(sortedInstances, func(i, j int) bool {
		return compareInstances(sortedInstances[i], sortedInstances[j])
	})

	return sortedInstances
}

// compareInstances compares two InstanceType objects based on CPU, Memory, Pods, and Ephemeral Storage (ascending).
func compareInstances(a, b *ClusterAPIInstanceType) bool {
	priorityResources := []corev1.ResourceName{
		corev1.ResourceCPU,              // Highest priority
		corev1.ResourceMemory,           // Second priority (handles Gi, GB, Mi, MB)
		corev1.ResourcePods,             // Third priority
		corev1.ResourceEphemeralStorage, // Fourth priority (handles Gi, GB, Mi, MB)
	}

	for _, res := range priorityResources {
		valueA, existsA := a.Capacity[res]
		valueB, existsB := b.Capacity[res]

		// Treat missing values as zero (if not present)
		if !existsA {
			valueA = resource.MustParse("0")
		}
		if !existsB {
			valueB = resource.MustParse("0")
		}

		// Compare values using Quantity.Cmp() (-1 if A < B, 0 if A == B, 1 if A > B)
		if cmp := valueA.Cmp(valueB); cmp != 0 {
			return cmp < 0 // Sort in ascending order
		}
	}

	return false // If all values are equal, maintain original order
}
