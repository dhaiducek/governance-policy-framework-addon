// Copyright (c) 2020 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package statussync

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	policiesv1 "open-cluster-management.io/governance-policy-propagator/api/v1"
	"open-cluster-management.io/governance-policy-propagator/controllers/common"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const ControllerName string = "policy-status-sync"

var log = ctrl.Log.WithName(ControllerName)

// SetupWithManager sets up the controller with the Manager.
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&policiesv1.Policy{}).
		Watches(
			&source.Kind{Type: &corev1.Event{}},
			handler.EnqueueRequestsFromMapFunc(eventMapper),
			builder.WithPredicates(eventPredicateFuncs),
		).
		Named(ControllerName).
		Complete(r)
}

// blank assignment to verify that ReconcilePolicy implements reconcile.Reconciler
var _ reconcile.Reconciler = &PolicyReconciler{}

// ReconcilePolicy reconciles a Policy object
type PolicyReconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	HubClient             client.Client
	ManagedClient         client.Client
	HubRecorder           record.EventRecorder
	ManagedRecorder       record.EventRecorder
	Scheme                *runtime.Scheme
	ClusterNamespaceOnHub string
}

//+kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policies/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// This is required for the status lease for the addon framework
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list

// Reconcile reads that state of the cluster for a Policy object and makes changes based on the state read
// and what is in the Policy.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *PolicyReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues(
		"Request.Namespace", request.Namespace, "Request.Name", request.Name, "HubNamespace", r.ClusterNamespaceOnHub,
	)
	reqLogger.Info("Reconciling the policy")

	// Fetch the Policy instance
	instance := &policiesv1.Policy{}

	err := r.ManagedClient.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// The replicated policy on the managed cluster was deleted.
			// check if it was deleted by user by checking if it still exists on hub
			hubInstance := &policiesv1.Policy{}
			err = r.HubClient.Get(
				ctx, types.NamespacedName{Namespace: r.ClusterNamespaceOnHub, Name: request.Name}, hubInstance,
			)

			if err != nil {
				if errors.IsNotFound(err) {
					// confirmed deleted on hub, doing nothing
					reqLogger.Info("Policy was deleted, no status to update")

					return reconcile.Result{}, nil
				}
				// other error, requeue
				reqLogger.Error(err, "Failed to get the policy, will requeue the request")

				return reconcile.Result{}, err
			}

			// still exist on hub, recover policy on managed
			managedInstance := hubInstance.DeepCopy()
			managedInstance.Namespace = request.Namespace

			if managedInstance.Labels["policy.open-cluster-management.io/cluster-namespace"] != "" {
				managedInstance.Labels["policy.open-cluster-management.io/cluster-namespace"] = request.Namespace
			}

			managedInstance.SetOwnerReferences(nil)
			managedInstance.SetResourceVersion("")

			return reconcile.Result{}, r.ManagedClient.Create(ctx, managedInstance)
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "Error reading the policy object, will requeue the request")

		return reconcile.Result{}, err
	}
	// get hub policy
	hubPlc := &policiesv1.Policy{}
	err = r.HubClient.Get(ctx, types.NamespacedName{Namespace: r.ClusterNamespaceOnHub, Name: request.Name}, hubPlc)

	if err != nil {
		// hub policy not found, it has been deleted
		if errors.IsNotFound(err) {
			reqLogger.Info("Hub policy not found, it has been deleted")
			// try to delete local one
			err = r.ManagedClient.Delete(ctx, instance)
			if err == nil || errors.IsNotFound(err) {
				// no err or err is not found means local policy has been deleted
				reqLogger.Info("Managed policy was deleted")

				return reconcile.Result{}, nil
			}
			// otherwise requeue to delete again
			reqLogger.Error(err, "Failed to delete the managed policy, will requeue the request")

			return reconcile.Result{}, err
		}

		reqLogger.Error(err, "Failed to get policy on hub")

		return reconcile.Result{}, err
	}
	// found, ensure managed plc matches hub plc
	if !common.CompareSpecAndAnnotation(instance, hubPlc) {
		// plc mismatch, update to latest
		instance.SetAnnotations(hubPlc.GetAnnotations())
		instance.Spec = hubPlc.Spec
		// update and stop here
		reqLogger.Info("Found mismatch with hub and managed policies, updating")

		return reconcile.Result{}, r.ManagedClient.Update(ctx, instance)
	}

	// plc matches hub plc, then get events
	eventList := &corev1.EventList{}
	err = r.ManagedClient.List(ctx, eventList, client.InNamespace(instance.GetNamespace()))

	if err != nil {
		// there is an error to list events, requeue
		reqLogger.Error(err, "Error listing events, will requeue the request")

		return reconcile.Result{}, err
	}
	// filter events to current policy instance and build map
	eventForPolicyMap := make(map[string]*[]policiesv1.ComplianceHistory)
	// panic if regexp invalid
	rgx := regexp.MustCompile(`(?i)^policy:\s*([A-Za-z0-9.-]+)\s*\/([A-Za-z0-9.-]+)`)
	for _, event := range eventList.Items {
		// sample event.Reason -- reason: 'policy: calamari/policy-grc-rbactest-example'
		reason := rgx.FindString(event.Reason)
		if event.InvolvedObject.Kind == policiesv1.Kind && event.InvolvedObject.APIVersion == policiesv1APIVersion &&
			event.InvolvedObject.Name == instance.GetName() && reason != "" {
			templateName := rgx.FindStringSubmatch(event.Reason)[2]
			eventHistory := policiesv1.ComplianceHistory{
				LastTimestamp: event.LastTimestamp,
				Message:       strings.TrimSpace(strings.TrimPrefix(event.Message, "(combined from similar events):")),
				EventName:     event.GetName(),
			}

			if eventForPolicyMap[templateName] == nil {
				eventForPolicyMap[templateName] = &[]policiesv1.ComplianceHistory{}
			}

			templateEvents := append(*eventForPolicyMap[templateName], eventHistory)
			eventForPolicyMap[templateName] = &templateEvents
		}
	}

	oldStatus := *instance.Status.DeepCopy()
	newStatus := policiesv1.PolicyStatus{}

	reqLogger.Info("Updating status for policy templates")

	for _, policyT := range instance.Spec.PolicyTemplates {
		object, _, err := unstructured.UnstructuredJSONScheme.Decode(policyT.ObjectDefinition.Raw, nil, nil)
		if err != nil {
			// failed to decode PolicyTemplate, skipping it
			reqLogger.Error(err, "Failed to decode policy template, skipping it")

			break
		}

		tName := object.(metav1.Object).GetName()
		existingDpt := &policiesv1.DetailsPerTemplate{}
		// retrieve existingDpt from instance.status.details field
		found := false

		for _, dpt := range instance.Status.Details {
			if dpt.TemplateMeta.Name == tName {
				// found existing status for policyTemplate
				// retrieve it
				existingDpt = dpt
				found = true

				reqLogger.Info("Found existing status, retrieving it", "PolicyTemplate", tName)

				break
			}
		}
		// no dpt from status field, initialize it
		if !found {
			existingDpt = &policiesv1.DetailsPerTemplate{
				TemplateMeta: metav1.ObjectMeta{
					Name: tName,
				},
				History: []policiesv1.ComplianceHistory{},
			}
		}

		history := []policiesv1.ComplianceHistory{}
		if eventForPolicyMap[tName] != nil {
			history = *eventForPolicyMap[tName]
		}

		for _, ech := range existingDpt.History {
			exists := false

			for _, ch := range history {
				if ch.LastTimestamp.Time.Equal(ech.LastTimestamp.Time) && ch.EventName == ech.EventName {
					// do nothing
					exists = true

					break
				}
			}
			// doesn't exists, append to history
			if !exists {
				history = append(history, ech)
			}
		}
		// sort by lasttimestamp
		sort.Slice(history, func(i, j int) bool {
			return history[i].LastTimestamp.Time.After(history[j].LastTimestamp.Time)
		})
		// remove duplicates
		newHistory := []policiesv1.ComplianceHistory{}

		for historyIndex := 0; historyIndex < len(history); historyIndex++ {
			newHistory = append(newHistory, history[historyIndex])

			for j := historyIndex; j < len(history); j++ {
				if history[historyIndex].EventName == history[j].EventName &&
					history[historyIndex].Message == history[j].Message {
					// same event, filter it
				} else {
					historyIndex = j - 1

					break
				}
			}
		}
		// shorten it to first 10
		size := 10
		if len(newHistory) < 10 {
			size = len(newHistory)
		}

		existingDpt.History = newHistory[0:size]

		// set compliancy at different level
		if len(existingDpt.History) > 0 {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(
				strings.TrimPrefix(existingDpt.History[0].Message, "(combined from similar events):"))), "compliant") {
				existingDpt.ComplianceState = policiesv1.Compliant
			} else {
				existingDpt.ComplianceState = policiesv1.NonCompliant
			}
		}

		// append existingDpt to status
		newStatus.Details = append(newStatus.Details, existingDpt)

		reqLogger.Info("Status update complete", "PolicyTemplate", tName)
	}

	instance.Status = newStatus
	// one violation found in status of one template, set overall compliancy to NonCompliant
	isCompliant := true

	for _, dpt := range newStatus.Details {
		if dpt.ComplianceState == "NonCompliant" {
			instance.Status.ComplianceState = policiesv1.NonCompliant
			isCompliant = false

			break
		} else if dpt.ComplianceState == "" {
			isCompliant = false
		}
	}
	// set to compliant only when all templates are compliant
	if isCompliant {
		instance.Status.ComplianceState = policiesv1.Compliant
	}

	// all done, update status on managed and hub
	// instance.Status.Details = nil
	if !equality.Semantic.DeepEqual(newStatus.Details, oldStatus.Details) ||
		instance.Status.ComplianceState != oldStatus.ComplianceState {
		reqLogger.Info("status mismatch on managed, update it")

		err = r.ManagedClient.Status().Update(ctx, instance)

		if err != nil {
			reqLogger.Error(err, "Failed to get update policy status on managed")

			return reconcile.Result{}, err
		}

		r.ManagedRecorder.Event(instance, "Normal", "PolicyStatusSync",
			fmt.Sprintf("Policy %s status was updated in cluster namespace %s", instance.GetName(),
				instance.GetNamespace()))
	} else {
		reqLogger.Info("status match on managed, nothing to update")
	}

	if os.Getenv("ON_MULTICLUSTERHUB") != "true" && !equality.Semantic.DeepEqual(hubPlc.Status, instance.Status) {
		reqLogger.Info("status not in sync, update the hub")

		hubPlc.Status = instance.Status
		err = r.HubClient.Status().Update(ctx, hubPlc)

		if err != nil {
			reqLogger.Error(err, "Failed to get update policy status on hub")

			return reconcile.Result{}, err
		}

		r.HubRecorder.Event(hubPlc, "Normal", "PolicyStatusSync",
			fmt.Sprintf("Policy %s status was updated in cluster namespace %s", hubPlc.GetName(),
				hubPlc.GetNamespace()))
	} else {
		reqLogger.Info("status match on hub, nothing to update")
	}

	reqLogger.Info("Reconciling complete")

	return reconcile.Result{}, nil
}
