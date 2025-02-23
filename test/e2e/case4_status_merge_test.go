// Copyright (c) 2020 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	policiesv1 "open-cluster-management.io/governance-policy-propagator/api/v1"
	"open-cluster-management.io/governance-policy-propagator/test/utils"
)

const (
	case4PolicyName string = "case4-test-policy"
	case4PolicyYaml string = "../resources/case4_status_merge/case4-test-policy.yaml"
)

var _ = Describe("Test status sync with multiple templates", func() {
	BeforeEach(func() {
		By("Creating a policy on hub cluster in ns:" + clusterNamespaceOnHub)
		_, err := kubectlHub("apply", "-f", case4PolicyYaml, "-n", clusterNamespaceOnHub)
		Expect(err).Should(BeNil())
		hubPlc := utils.GetWithTimeout(
			clientHubDynamic,
			gvrPolicy,
			case4PolicyName,
			clusterNamespaceOnHub,
			true,
			defaultTimeoutSeconds)
		Expect(hubPlc).NotTo(BeNil())
		managedPlc := utils.GetWithTimeout(
			clientManagedDynamic,
			gvrPolicy,
			case4PolicyName,
			clusterNamespace,
			true,
			defaultTimeoutSeconds)
		Expect(managedPlc).NotTo(BeNil())
	})
	AfterEach(func() {
		By("Deleting a policy on hub cluster in ns:" + clusterNamespaceOnHub)
		_, err := kubectlHub(
			"delete",
			"-f",
			case4PolicyYaml,
			"-n",
			clusterNamespaceOnHub,
		)
		Expect(err).Should(BeNil())
		opt := metav1.ListOptions{}
		utils.ListWithTimeout(
			clientHubDynamic,
			gvrPolicy,
			opt,
			0,
			true,
			defaultTimeoutSeconds)
		utils.ListWithTimeout(
			clientManagedDynamic,
			gvrPolicy,
			opt,
			0,
			true,
			defaultTimeoutSeconds)
		By("clean up all events")
		_, err = kubectlManaged(
			"delete",
			"events",
			"-n",
			clusterNamespace,
			"--all",
		)
		Expect(err).Should(BeNil())
	})
	It("Should merge existing status with new status from event", func() {
		By("Generating some events in ns:" + clusterNamespace)
		managedPlc := utils.GetWithTimeout(
			clientManagedDynamic,
			gvrPolicy,
			case4PolicyName,
			clusterNamespace,
			true,
			defaultTimeoutSeconds)
		managedRecorder.Event(
			managedPlc,
			"Normal",
			"policy: managed/case4-test-policy-configurationpolicy",
			"Compliant; No violation detected")
		By("Checking if policy status is noncompliant")
		Eventually(func() interface{} {
			managedPlc = utils.GetWithTimeout(
				clientManagedDynamic,
				gvrPolicy,
				case4PolicyName,
				clusterNamespace,
				true,
				defaultTimeoutSeconds)

			return getCompliant(managedPlc)
		}, defaultTimeoutSeconds, 1).Should(Equal("Compliant"))
		By("Delete events in ns:" + clusterNamespace)
		_, err := kubectlManaged(
			"delete",
			"event",
			"-n",
			clusterNamespace,
			"--all",
		)
		Expect(err).Should(BeNil())
		utils.ListWithTimeout(
			clientManagedDynamic,
			gvrEvent,
			metav1.ListOptions{FieldSelector: "involvedObject.name=case4-test-policy,reason!=PolicyStatusSync"},
			0,
			true,
			defaultTimeoutSeconds)
		By("Generating some new events in ns:" + clusterNamespace)
		managedRecorder.Event(
			managedPlc,
			"Warning",
			"policy: managed/case4-test-policy-configurationpolicy",
			"NonCompliant; Violation detected")
		managedRecorder.Event(
			managedPlc,
			"Normal",
			"policy: managed/case4-test-policy-configurationpolicy",
			"Compliant; No violation detected")
		By("Checking if history size = 3")
		var plc *policiesv1.Policy
		Eventually(func() interface{} {
			managedPlc = utils.GetWithTimeout(
				clientManagedDynamic,
				gvrPolicy,
				case4PolicyName,
				clusterNamespace,
				true,
				defaultTimeoutSeconds)
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(managedPlc.Object, &plc)
			Expect(err).To(BeNil())
			Expect(plc.Status.Details[0].TemplateMeta.GetName()).To(Equal("case4-test-policy-configurationpolicy"))

			return len(plc.Status.Details[0].History)
		}, defaultTimeoutSeconds, 1).Should(Equal(3))
	})
})
