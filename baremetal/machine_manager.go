/*
Copyright 2019 The Kubernetes Authors.

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

package baremetal

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	_ "github.com/go-logr/logr"
	pkgerrors "github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"

	bmh "github.com/metal3-io/baremetal-operator/pkg/apis/metal3/v1alpha1"
	capbm "sigs.k8s.io/cluster-api-provider-baremetal/api/v1alpha2"
	capi "sigs.k8s.io/cluster-api/api/v1alpha2"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ProviderName = "baremetal"
	// HostAnnotation is the key for an annotation that should go on a Machine to
	// reference what BareMetalHost it corresponds to.
	HostAnnotation = "metal3.io/BareMetalHost"
	requeueAfter   = time.Second * 30
)

// MachineManager is responsible for performing machine reconciliation
type MachineManager struct {
	client      client.Client
	patchHelper *patch.Helper

	Cluster          *capi.Cluster
	BareMetalCluster *capbm.BareMetalCluster
	Machine          *capi.Machine
	BareMetalMachine *capbm.BareMetalMachine
	// log          logr.Logger
}

// NewMachineManager returns a new helper for managing a cluster with a given name.
func newMachineManager(client client.Client,
	cluster *capi.Cluster, baremetalCluster *capbm.BareMetalCluster,
	machine *capi.Machine, baremetalMachine *capbm.BareMetalMachine) (*MachineManager, error) {

	helper, err := patch.NewHelper(machine, client)
	if err != nil {
		return nil, pkgerrors.Wrap(err, "failed to init patch helper")
	}

	return &MachineManager{
		client:      client,
		patchHelper: helper,

		Cluster:          cluster,
		BareMetalCluster: baremetalCluster,
		Machine:          machine,
		BareMetalMachine: baremetalMachine,
	}, nil
}

// Name returns the BareMetalMachine name.
func (m *MachineManager) Name() string {
	return m.BareMetalMachine.Name
}

// Namespace returns the namespace name.
func (m *MachineManager) Namespace() string {
	return m.BareMetalMachine.Namespace
}

// IsControlPlane returns true if the machine is a control plane.
func (m *MachineManager) IsControlPlane() bool {
	return util.IsControlPlaneMachine(m.Machine)
}

// Role returns the machine role from the labels.
func (m *MachineManager) Role() string {
	if util.IsControlPlaneMachine(m.Machine) {
		return "control-plane"
	}
	return "node"
}

// ProviderID return the provider identifier for this machine
func (m *MachineManager) GetProviderID() string {
	if m.BareMetalMachine.Spec.ProviderID != nil {
		return *m.BareMetalMachine.Spec.ProviderID
	}
	return ""
}

// SetNodeProviderID sets the docker provider ID for the kubernetes node
func (m *MachineManager) SetProviderID(v string) {
	m.BareMetalMachine.Spec.ProviderID = pointer.StringPtr(v)
}

// SetReady sets the AzureMachine Ready Status
func (m *MachineManager) SetReady() {
	m.BareMetalMachine.Status.Ready = true
}

// SetAnnotation sets a key value annotation on the BareMetalMachine.
func (m *MachineManager) SetAnnotation(key, value string) {
	if m.BareMetalMachine.Annotations == nil {
		m.BareMetalMachine.Annotations = map[string]string{}
	}
	m.BareMetalMachine.Annotations[key] = value
}

// Close the MachineManager by updating the machine spec, machine status.
func (m *MachineManager) Close() error {
	return m.patchHelper.Patch(context.TODO(), m.BareMetalMachine)
}

// ExecBootstrap runs bootstrap on a node, this is generally `kubeadm <init|join>`
func (m *MachineManager) ExecBootstrap(data string) error {
	return nil
}

// KubeadmReset will run `kubeadm reset` on the machine.
func (m *MachineManager) KubeadmReset() error {
	return nil
}

// Create creates a machine and is invoked by the Machine Controller
func (mgr *MachineManager) Create(ctx context.Context) (string, error) {
	log.Printf("Creating machine %v .", mgr.Machine.Name)
	providerId := ""

	// load and validate the config
	if mgr.BareMetalMachine == nil {
		// Should have been picked earlier. Do not requeue
		return providerId, nil
	}

	config := mgr.BareMetalMachine.Spec
	err := config.IsValid()
	if err != nil {
		// Should have been picked earlier. Do not requeue
		mgr.setError(ctx, err.Error())
		return providerId, nil
	}

	// clear an error if one was previously set
	mgr.clearError(ctx)

	// look for associated BMH
	host, err := mgr.getHost(ctx)
	if err != nil {
		return providerId, err
	}

	// none found, so try to choose one
	if host == nil {
		host, err = mgr.chooseHost(ctx)
		if err != nil {
			return providerId, err
		}
		if host == nil {
			log.Printf("No available host found. Requeuing.")
			return providerId, &RequeueAfterError{RequeueAfter: requeueAfter}
		}
		log.Printf("Associating machine %s with host %s", mgr.Machine.Name, host.Name)
	} else {
		log.Printf("Machine %s already associated with host %s", mgr.Machine.Name, host.Name)
	}

	providerId = host.Name
	err = mgr.setHostSpec(ctx, host)
	if err != nil {
		return providerId, err
	}

	err = mgr.ensureAnnotation(ctx, host)
	if err != nil {
		return providerId, err
	}

	if err := mgr.updateMachineStatus(ctx, host); err != nil {
		return providerId, err
	}

	log.Printf("Finished creating machine %v .", mgr.Machine.Name)
	return providerId, nil
}

// Delete deletes a machine and is invoked by the Machine Controller
func (mgr *MachineManager) Delete(ctx context.Context) (string, error) {
	log.Printf("Deleting machine %v .", mgr.Machine.Name)
	providerId := ""

	host, err := mgr.getHost(ctx)
	if err != nil {
		return providerId, err
	}
	if host != nil && host.Spec.ConsumerRef != nil {
		// don't remove the ConsumerRef if it references some other machine
		if !consumerRefMatches(host.Spec.ConsumerRef, mgr.Machine) {
			log.Printf("host associated with %v, not machine %v.",
				host.Spec.ConsumerRef.Name, mgr.Machine.Name)
			return providerId, nil
		}

		providerId = host.Name
		if host.Spec.Image != nil || host.Spec.Online || host.Spec.UserData != nil {
			host.Spec.Image = nil
			host.Spec.Online = false
			host.Spec.UserData = nil
			err = mgr.client.Update(ctx, host)
			if err != nil && !errors.IsNotFound(err) {
				return host.Name, err
			}
			return providerId, &RequeueAfterError{}
		}

		waiting := true
		switch host.Status.Provisioning.State {
		case bmh.StateRegistrationError, bmh.StateRegistering,
			bmh.StateMatchProfile, bmh.StateInspecting,
			bmh.StateReady, bmh.StateValidationError:
			// Host is not provisioned
			waiting = false
		case bmh.StateExternallyProvisioned:
			// We have no control over provisioning, so just wait until the
			// host is powered off
			waiting = host.Status.PoweredOn
		}
		if waiting {
			return providerId, &RequeueAfterError{RequeueAfter: requeueAfter}
		} else {
			host.Spec.ConsumerRef = nil
			err = mgr.client.Update(ctx, host)
			if err != nil && !errors.IsNotFound(err) {
				return providerId, err
			}
		}
	}
	log.Printf("finished deleting machine %v.", mgr.Machine.Name)
	return providerId, nil
}

// Update updates a machine and is invoked by the Machine Controller
func (mgr *MachineManager) Update(ctx context.Context) (string, error) {
	log.Printf("Updating machine %v .", mgr.Machine.Name)
	providerId := ""

	// clear any error message that was previously set. This method doesn't set
	// error messages yet, so we know that it's incorrect to have one here.
	mgr.clearError(ctx)

	host, err := mgr.getHost(ctx)
	if err != nil {
		return providerId, err
	}
	if host == nil {
		return providerId, fmt.Errorf("host not found for machine %s", mgr.Machine.Name)
	}

	providerId = host.Name
	err = mgr.ensureAnnotation(ctx, host)
	if err != nil {
		return providerId, err
	}

	if err := mgr.updateMachineStatus(ctx, host); err != nil {
		return providerId, err
	}

	log.Printf("Finished updating machine %v .", mgr.Machine.Name)
	return providerId, nil
}

// Exists tests for the existence of a machine and is invoked by the Machine Controller
func (mgr *MachineManager) Exists(ctx context.Context) (bool, error) {
	log.Printf("Checking if machine %v exists.", mgr.Machine.Name)
	host, err := mgr.getHost(ctx)
	if err != nil {
		return false, err
	}
	if host == nil {
		log.Printf("Machine %v does not exist.", mgr.Machine.Name)
		return false, nil
	}
	log.Printf("Machine %v exists.", mgr.Machine.Name)
	return true, nil
}

// The Machine Actuator interface must implement GetIP and GetKubeConfig functions as a workaround for issues
// cluster-api#158 (https://github.com/kubernetes-sigs/cluster-api/issues/158) and cluster-api#160
// (https://github.com/kubernetes-sigs/cluster-api/issues/160).

// GetIP returns IP address of the machine in the cluster.
func (mgr *MachineManager) GetIP() (string, error) {
	log.Printf("Getting IP of machine %v .", mgr.Machine.Name)
	return "", fmt.Errorf("TODO: Not yet implemented")
}

// GetKubeConfig gets a kubeconfig from the running control plane.
func (mgr *MachineManager) GetKubeConfig() (string, error) {
	log.Printf("Getting IP of machine %v .", mgr.Machine.Name)
	return "", fmt.Errorf("TODO: Not yet implemented")
}

// getHost gets the associated host by looking for an annotation on the machine
// that contains a reference to the host. Returns nil if not found. Assumes the
// host is in the same namespace as the machine.
func (mgr *MachineManager) getHost(ctx context.Context) (*bmh.BareMetalHost, error) {
	annotations := mgr.Machine.ObjectMeta.GetAnnotations()
	if annotations == nil {
		return nil, nil
	}
	hostKey, ok := annotations[HostAnnotation]
	if !ok {
		return nil, nil
	}
	hostNamespace, hostName, err := cache.SplitMetaNamespaceKey(hostKey)
	if err != nil {
		log.Printf("Error parsing annotation value \"%s\": %v", hostKey, err)
		return nil, err
	}

	host := bmh.BareMetalHost{}
	key := client.ObjectKey{
		Name:      hostName,
		Namespace: hostNamespace,
	}
	err = mgr.client.Get(ctx, key, &host)
	if errors.IsNotFound(err) {
		log.Printf("Annotated host %s not found", hostKey)
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &host, nil
}

// chooseHost iterates through known hosts and returns one that can be
// associated with the machine. It searches all hosts in case one already has an
// association with this machine.
func (mgr *MachineManager) chooseHost(ctx context.Context) (*bmh.BareMetalHost, error) {

	// get list of BMH
	hosts := bmh.BareMetalHostList{}
	opts := &client.ListOptions{
		Namespace: mgr.Machine.Namespace,
	}

	err := mgr.client.List(ctx, &hosts, opts)
	if err != nil {
		return nil, err
	}

	// Using the label selector on ListOptions above doesn't seem to work.
	// I think it's because we have a local cache of all BareMetalHosts.
	labelSelector := labels.NewSelector()
	var reqs labels.Requirements
	for labelKey, labelVal := range mgr.BareMetalMachine.Spec.HostSelector.MatchLabels {
		log.Printf("Adding requirement to match label: '%s' == '%s'", labelKey, labelVal)
		r, err := labels.NewRequirement(labelKey, selection.Equals, []string{labelVal})
		if err != nil {
			log.Printf("Failed to create MatchLabel requirement, not choosing host: %v", err)
			return nil, err
		}
		reqs = append(reqs, *r)
	}
	for _, req := range mgr.BareMetalMachine.Spec.HostSelector.MatchExpressions {
		log.Printf("Adding requirement to match label: '%s' %s '%s'", req.Key, req.Operator, req.Values)
		lowercaseOperator := selection.Operator(strings.ToLower(string(req.Operator)))
		r, err := labels.NewRequirement(req.Key, lowercaseOperator, req.Values)
		if err != nil {
			log.Printf("Failed to create MatchExpression requirement, not choosing host: %v", err)
			return nil, err
		}
		reqs = append(reqs, *r)
	}
	labelSelector = labelSelector.Add(reqs...)

	availableHosts := []*bmh.BareMetalHost{}

	for i, host := range hosts.Items {
		if host.Available() {
			if labelSelector.Matches(labels.Set(host.ObjectMeta.Labels)) {
				log.Printf("Host '%s' matched hostSelector for Machine '%s'", host.Name, mgr.Machine.Name)
				availableHosts = append(availableHosts, &hosts.Items[i])
			} else {
				log.Printf("Host '%s' did not match hostSelector for Machine '%s'", host.Name, mgr.Machine.Name)
			}
		} else if host.Spec.ConsumerRef != nil && consumerRefMatches(host.Spec.ConsumerRef, mgr.Machine) {
			log.Printf("found host %s with existing ConsumerRef", host.Name)
			return &hosts.Items[i], nil
		}
	}
	log.Printf("%d hosts available while choosing host for machine '%s'", len(availableHosts), mgr.Machine.Name)
	if len(availableHosts) == 0 {
		return nil, nil
	}

	// choose a host at random from available hosts
	rand.Seed(time.Now().Unix())
	chosenHost := availableHosts[rand.Intn(len(availableHosts))]

	return chosenHost, nil
}

// consumerRefMatches returns a boolean based on whether the consumer
// reference and machine metadata match
func consumerRefMatches(consumer *corev1.ObjectReference, machine *capi.Machine) bool {
	if consumer.Name != machine.Name {
		return false
	}
	if consumer.Namespace != machine.Namespace {
		return false
	}
	if consumer.Kind != machine.Kind {
		return false
	}
	if consumer.APIVersion != machine.APIVersion {
		return false
	}
	return true
}

// setHostSpec will ensure the host's Spec is set according to the machine's
// details. It will then update the host via the kube API. If UserData does not
// include a Namespace, it will default to the Machine's namespace.
func (mgr *MachineManager) setHostSpec(ctx context.Context, host *bmh.BareMetalHost) error {

	// We only want to update the image setting if the host does not
	// already have an image.
	//
	// A host with an existing image is already provisioned and
	// upgrades are not supported at this time. To re-provision a
	// host, we must fully deprovision it and then provision it again.
	if host.Spec.Image == nil {
		host.Spec.Image = &bmh.Image{
			URL:      mgr.BareMetalMachine.Spec.Image.URL,
			Checksum: mgr.BareMetalMachine.Spec.Image.Checksum,
		}
		host.Spec.UserData = mgr.BareMetalMachine.Spec.UserData
		if host.Spec.UserData != nil && host.Spec.UserData.Namespace == "" {
			host.Spec.UserData.Namespace = mgr.Machine.Namespace
		}
	}

	host.Spec.ConsumerRef = &corev1.ObjectReference{
		Kind:       "Machine",
		Name:       mgr.Machine.Name,
		Namespace:  mgr.Machine.Namespace,
		APIVersion: mgr.Machine.APIVersion,
	}

	host.Spec.Online = true
	return mgr.client.Update(ctx, host)
}

// ensureAnnotation makes sure the machine has an annotation that references the
// host and uses the API to update the machine if necessary.
func (mgr *MachineManager) ensureAnnotation(ctx context.Context, host *bmh.BareMetalHost) error {
	annotations := mgr.BareMetalMachine.ObjectMeta.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	hostKey, err := cache.MetaNamespaceKeyFunc(host)
	if err != nil {
		log.Printf("Error parsing annotation value \"%s\": %v", hostKey, err)
		return err
	}
	existing, ok := annotations[HostAnnotation]
	if ok {
		if existing == hostKey {
			return nil
		}
		log.Printf("Warning: found stray annotation for host %s on machine %s. Overwriting.", existing, mgr.BareMetalMachine.Name)
	}
	annotations[HostAnnotation] = hostKey
	mgr.BareMetalMachine.ObjectMeta.SetAnnotations(annotations)
	// Will be done by mgr.Close()
	// return mgr.client.Update(ctx, mgr.BareMetalMachine)
	return nil
}

// setError sets the ErrorMessage and ErrorReason fields on the machine and logs
// the message. It assumes the reason is invalid configuration, since that is
// currently the only relevant MachineStatusError choice.
func (mgr *MachineManager) setError(ctx context.Context, message string) {
	mgr.BareMetalMachine.Status.ErrorMessage = &message
	reason := capierrors.InvalidConfigurationMachineError
	mgr.BareMetalMachine.Status.ErrorReason = &reason
}

// clearError removes the ErrorMessage from the machine's Status if set. Returns
// nil if ErrorMessage was already nil. Returns a RequeueAfterError if the
// machine was updated.
func (mgr *MachineManager) clearError(ctx context.Context) {
	if mgr.BareMetalMachine.Status.ErrorMessage != nil || mgr.BareMetalMachine.Status.ErrorReason != nil {
		mgr.BareMetalMachine.Status.ErrorMessage = nil
		mgr.BareMetalMachine.Status.ErrorReason = nil
	}
}

// updateMachineStatus updates a machine object's status.
func (mgr *MachineManager) updateMachineStatus(ctx context.Context, host *bmh.BareMetalHost) error {
	addrs := mgr.nodeAddresses(host)

	machineCopy := mgr.BareMetalMachine.DeepCopy()
	machineCopy.Status.Addresses = addrs

	if equality.Semantic.DeepEqual(mgr.Machine.Status, machineCopy.Status) {
		// Status did not change
		return nil
	}

	now := metav1.Now()
	mgr.BareMetalMachine.Status.LastUpdated = &now
	mgr.BareMetalMachine.Status.Addresses = addrs

	return nil
}

// NodeAddresses returns a slice of corev1.NodeAddress objects for a
// given Baremetal machine.
func (mgr *MachineManager) nodeAddresses(host *bmh.BareMetalHost) []capi.MachineAddress {
	addrs := []capi.MachineAddress{}

	// If the host is nil or we have no hw details, return an empty address array.
	if host == nil || host.Status.HardwareDetails == nil {
		return addrs
	}

	for _, nic := range host.Status.HardwareDetails.NIC {
		address := capi.MachineAddress{
			Type:    capi.MachineInternalIP,
			Address: nic.IP,
		}
		addrs = append(addrs, address)
	}

	if host.Status.HardwareDetails.Hostname != "" {
		addrs = append(addrs, capi.MachineAddress{
			Type:    capi.MachineHostName,
			Address: host.Status.HardwareDetails.Hostname,
		})
		addrs = append(addrs, capi.MachineAddress{
			Type:    capi.MachineInternalDNS,
			Address: host.Status.HardwareDetails.Hostname,
		})
	}

	return addrs
}