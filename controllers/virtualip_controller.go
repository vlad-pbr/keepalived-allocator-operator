/*
Copyright 2021.

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
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	paasv1 "github.com/vlad-pbr/keepalived-allocator-operator/api/v1"
)

// VirtualIPReconciler reconciles a VirtualIP object
type VirtualIPReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// get env variable or fallback if not defined
func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if len(value) == 0 {
		return fallback
	}
	return value
}

var groupSegmentMappingLabel = "gsm"
var keepalivedGroupNamespace = getEnv("KEEPALIVED_GROUP_NAMESPACE", "keepalived-operator")

func (r *VirtualIPReconciler) getService(virtualIP *paasv1.VirtualIP) (*corev1.Service, error) {

	// get the service
	service := &corev1.Service{}
	err := r.Client.Get(context.Background(), client.ObjectKey{
		Namespace: virtualIP.Namespace,
		Name:      virtualIP.Status.Service,
	}, service)
	if err != nil {
		return nil, err
	}

	// return service
	return service, nil
}

func (r *VirtualIPReconciler) cloneService(virtualIP *paasv1.VirtualIP, clone *corev1.Service) (*corev1.Service, error) {

	// update the new service
	clone.Name = fmt.Sprintf("%s-keepalived-clone", clone.Name)
	clone.Spec.ClusterIP = ""
	clone.ResourceVersion = ""

	// set owner reference
	clone.OwnerReferences = nil
	err := controllerutil.SetOwnerReference(virtualIP, clone, r.Scheme)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("received error while setting service's owner: %v", err))
	}

	return clone, nil
}

func (r *VirtualIPReconciler) patchService(service *corev1.Service, ip string, keepalivedGroup string, remove bool) {

	// initialize annotations if needed
	if service.Annotations == nil {
		service.Annotations = make(map[string]string)
	}

	if !remove {
		// annotate service with keepalived group annotation
		service.Annotations["keepalived-operator.redhat-cop.io/keepalivedgroup"] =
			fmt.Sprintf("%s/%s", keepalivedGroupNamespace, keepalivedGroup)

		// set IP within ExternalIPs field
		service.Spec.ExternalIPs = []string{ip}
	} else {
		delete(service.Annotations, "keepalived-operator.redhat-cop.io/keepalivedgroup")
		service.Spec.ExternalIPs = []string{}
	}
}

func (r *VirtualIPReconciler) labelIP(ipObject *paasv1.IP, virtualIP *paasv1.VirtualIP, gsmName string) {

	// set appropriate labels and annotations
	ipObject.Labels = map[string]string{groupSegmentMappingLabel: gsmName}
	ipObject.Annotations = map[string]string{
		"virtualips.paas.il/owner": client.ObjectKeyFromObject(virtualIP).String(),
	}

}

func (r *VirtualIPReconciler) reserveIP(groupSegmentMapping *paasv1.GroupSegmentMapping, virtualIP *paasv1.VirtualIP) (string, error) {

	// get list of available IPs within the cluster
	availableIPs, err := r.getAvailableIPs(groupSegmentMapping)
	if err != nil {
		return "", err
	}

	// try to reserve an IP until we run out of IPs
	for _, ip := range availableIPs {

		// initialize IP object
		ipObject := &paasv1.IP{
			ObjectMeta: metav1.ObjectMeta{
				Name: ip,
			},
		}
		r.labelIP(ipObject, virtualIP, groupSegmentMapping.Name)

		// try creating IP object
		err := r.Create(context.Background(), ipObject)

		// no error - no problem
		if err == nil {
			return ip, nil

			// if error and it's not AlreadyExists error - report
		} else if err != nil && !apierrors.IsAlreadyExists(err) {
			return "", errors.New(fmt.Sprintf("an error occurred while allocating IP: %v", err))
		}
	}

	// could not allocate
	return "", errors.New("there are no available IPs")
}

func (r *VirtualIPReconciler) getAvailableIPs(groupSegmentMapping *paasv1.GroupSegmentMapping) ([]string, error) {

	// list allocated IPs from given GSM
	IPList := &paasv1.IPList{}
	selector := labels.SelectorFromSet(map[string]string{groupSegmentMappingLabel: groupSegmentMapping.Name})
	if err := r.List(context.Background(), IPList, &client.ListOptions{LabelSelector: selector}); err != nil {
		return nil, err
	}

	// gather a list of IPs we can't use
	excludedIPs := []string{}
	for _, IP := range IPList.Items {
		excludedIPs = append(excludedIPs, IP.Name)
	}
	for _, ip := range groupSegmentMapping.Spec.ExcludedIPs {
		excludedIPs = append(excludedIPs, ip)
	}

	// parse GSM's CIDR field
	ipAddress, ipnet, err := net.ParseCIDR(groupSegmentMapping.Spec.Segment)
	if err != nil {
		return nil, err
	}

	// filter out excluded IPs from segment
	var ips []string
	for ipAddress := ipAddress.Mask(ipnet.Mask).To4(); ipnet.Contains(ipAddress); incrementIP(ipAddress) {
		ip := ipAddress.String()
		if !contains(excludedIPs, ip) {
			ips = append(ips, ip)
		}
	}

	return ips, nil
}

func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		if ip[j] == 255 {
			ip[j] = 0
		} else {
			ip[j]++
			break
		}
	}
}

func contains(arr []string, str string) bool {
	for _, item := range arr {
		if item == str {
			return true
		}
	}
	return false
}

func (r *VirtualIPReconciler) getGSMs() (*[]paasv1.GroupSegmentMapping, error) {
	gsms := &paasv1.GroupSegmentMappingList{}
	if err := r.Client.List(context.Background(), gsms, &client.ListOptions{}); err != nil {
		r.Log.Error(err, err.Error())
		return nil, err
	}

	return &gsms.Items, nil
}

func (r *VirtualIPReconciler) getGSMBySegment(segment string) (*paasv1.GroupSegmentMapping, error) {

	GroupSegmentMappingList, err := r.getGSMs()
	if err != nil {
		r.Log.Error(err, err.Error())
		return nil, err
	}

	for _, gsm := range *GroupSegmentMappingList {
		if gsm.Spec.Segment == segment {
			return &gsm, nil
		}
	}

	err = errors.New("GroupSegmentMapping not found for the requested segment")
	r.Log.Error(err, err.Error())
	return nil, err
}

func (r *VirtualIPReconciler) allocateIP(virtualIP *paasv1.VirtualIP) (string, string, string, error) {

	var ip string
	var keepalivedGroup string
	var gsmName string

	// allocate IP from given segment
	if virtualIP.Spec.Segment != "" {

		// find matching GSM
		gsm, err := r.getGSMBySegment(virtualIP.Spec.Segment)
		if err != nil {
			return "", "", "", err
		}

		// reserve IP from given GSM
		ip, err = r.reserveIP(gsm, virtualIP)
		if err != nil {
			return "", "", "", err
		}

		// store keepalived group info
		keepalivedGroup = gsm.Spec.KeepalivedGroup
		gsmName = gsm.Name

		// allocate any available IP address
	} else {

		// get all GSMs
		gsms, err := r.getGSMs()
		if err != nil {
			return "", "", "", errors.New(fmt.Sprintf("failed to list GroupSegmentMappings: %v", err))
		}

		// iterate over all GSMs
		for _, gsm := range *gsms {

			// try reserving IP from given GSM
			ip, err = r.reserveIP(&gsm, virtualIP)
			if err != nil {
				return "", "", "", err
			}

			// store keepalived group info
			if ip != "" {
				keepalivedGroup = gsm.Spec.KeepalivedGroup
				gsmName = gsm.Name
				break
			}
		}
	}

	// make sure that we received a valid IP address
	if ip == "" {
		return "", "", "", errors.New("no IP could be allocated")
	}

	return ip, keepalivedGroup, gsmName, nil
}

func (r *VirtualIPReconciler) updateStatus(virtualIP *paasv1.VirtualIP, logger logr.Logger, e error) (ctrl.Result, error) {

	// log error if present
	if e != nil {
		logger.Error(e, "")
		virtualIP.Status.Message = e.Error()
		virtualIP.Status.State = paasv1.StateError
	}

	// do not update status of a VIP that is being deleted
	if virtualIP.DeletionTimestamp.IsZero() {

		if err := r.Status().Update(context.Background(), virtualIP); err != nil {
			return ctrl.Result{}, errors.New(fmt.Sprintf("failed to update VirtualIP status: %v", err))
		}
	}

	return ctrl.Result{}, nil
}

// +kubebuilder:rbac:groups=paas.org,resources=virtualips,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=paas.org,resources=virtualips/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=paas.org,resources=virtualips/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the VirtualIP object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *VirtualIPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("virtualip", req.NamespacedName)
	logger.Info("reconciling")

	// get current VIP from cluster
	virtualIP := &paasv1.VirtualIP{}
	err := r.Client.Get(context.Background(), req.NamespacedName, virtualIP)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// initialize variables
	deleteVIP := !virtualIP.DeletionTimestamp.IsZero()
	ipFinalizer := "ip.finalizers.virtualips.paas.org"
	serviceFinalizer := "service.finalizers.virtualips.paas.org"

	// delete IP object for the current VIP
	if deleteVIP {

		if controllerutil.ContainsFinalizer(virtualIP, ipFinalizer) {

			// remove IP object from cluster
			if err := r.Delete(context.Background(), &paasv1.IP{
				ObjectMeta: metav1.ObjectMeta{
					Name: virtualIP.Status.IP,
				},
			}); err != nil {
				return r.updateStatus(virtualIP, logger, err)
			}

			// remove IP finalizer and update
			controllerutil.RemoveFinalizer(virtualIP, ipFinalizer)
			if err := r.Update(context.Background(), virtualIP); err != nil {
				return r.updateStatus(virtualIP, logger, err)
			}

			return ctrl.Result{}, nil
		}

	} else {

		// allocate a new IP address if not present
		if virtualIP.Status.IP == "" {

			virtualIP.Status.IP, virtualIP.Status.KeepalivedGroup, virtualIP.Status.GSM, err = r.allocateIP(virtualIP)
			if err != nil {
				return r.updateStatus(virtualIP, logger, errors.New(fmt.Sprintf("could not allocate an IP: %v", err)))
			}

			// update status for the next cycle
			virtualIP.Status.Message = "creating IP object for the service"
			virtualIP.Status.State = paasv1.StateCreatingIP
			return r.updateStatus(virtualIP, logger, nil)
		}

		// ensure IP object existence
		ipObject := &paasv1.IP{
			ObjectMeta: metav1.ObjectMeta{
				Name: virtualIP.Status.IP,
			},
		}
		_, err := controllerutil.CreateOrUpdate(context.Background(), r.Client, ipObject, func() error {
			r.labelIP(ipObject, virtualIP, virtualIP.Status.GSM)
			return nil
		})
		if err != nil {
			return r.updateStatus(virtualIP, logger, errors.New(fmt.Sprintf("could not create/update an IP object: %v", err)))
		}

		// add finalizer for IP object
		if !controllerutil.ContainsFinalizer(virtualIP, ipFinalizer) {
			controllerutil.AddFinalizer(virtualIP, ipFinalizer)

			// update object finalizers
			if err := r.Update(context.Background(), virtualIP); err != nil {
				return r.updateStatus(virtualIP, logger, errors.New(fmt.Sprintf("could not add finalizer for IP object: %v", err)))
			}

			// end reconciliation cycle - the update above will trigger a new cycle
			return ctrl.Result{}, nil
		}
	}

	// decide on the service status
	if virtualIP.Status.Service == "" {
		virtualIP.Status.Service = virtualIP.Spec.Service
		virtualIP.Status.Clone = &virtualIP.Spec.Clone

		// update status for the next cycle
		virtualIP.Status.Message = "exposing service with an external IP"
		virtualIP.Status.State = paasv1.StateExposing
		return r.updateStatus(virtualIP, logger, nil)
	}

	// get service
	service, err := r.getService(virtualIP)
	if err != nil {
		return r.updateStatus(virtualIP, logger, errors.New(fmt.Sprintf("could not get service to be exposed: %v", err)))
	}

	// clone if specified
	if *virtualIP.Status.Clone {
		service, err = r.cloneService(virtualIP, service)
		if err != nil {
			return r.updateStatus(virtualIP, logger, errors.New(fmt.Sprintf("received error while cloning service: %v", err)))
		}
	}

	// do not update clone when deleting as it does not exist anymore
	if !(deleteVIP && *virtualIP.Status.Clone) {

		// patch and create/update service
		_, err = controllerutil.CreateOrUpdate(context.Background(), r.Client, service, func() error {
			r.patchService(service, virtualIP.Status.IP, virtualIP.Status.KeepalivedGroup, deleteVIP)
			return nil
		})
		if err != nil {
			return r.updateStatus(virtualIP, logger, errors.New(fmt.Sprintf("failed to create/update the service: %v", err)))
		}

	}

	// remove/add service finalizer if not present
	if deleteVIP || !controllerutil.ContainsFinalizer(virtualIP, serviceFinalizer) {

		if deleteVIP {
			controllerutil.RemoveFinalizer(virtualIP, serviceFinalizer)
		} else {
			controllerutil.AddFinalizer(virtualIP, serviceFinalizer)
		}

		// update object finalizers with service finalizer
		if err := r.Update(context.Background(), virtualIP); err != nil {
			return r.updateStatus(virtualIP, logger, errors.New(fmt.Sprintf("could not add finalizer for service: %v", err)))
		}

		return ctrl.Result{}, nil
	}

	virtualIP.Status.Message = "successfully allocated an IP address"
	virtualIP.Status.State = paasv1.StateValid
	return r.updateStatus(virtualIP, logger, nil)
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualIPReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&paasv1.VirtualIP{}).
		Complete(r)
}
