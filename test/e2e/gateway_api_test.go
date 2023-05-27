//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorclient "github.com/openshift/cluster-ingress-operator/pkg/operator/client"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/names"

	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	testHostname = "*.example.com"
)

var crdNames = []string{
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
	"referencegrants.gateway.networking.k8s.io",
}

// getEnabledFeatures returns the list of enabled features for the current
// cluster version (as reported in the clusterversions/version object) from the
// cluster featuresgates.
func getEnabledFeatures(t *testing.T) []configv1.FeatureGateName {
	var version configv1.ClusterVersion
	if err := wait.PollImmediate(1*time.Second, 1*time.Minute, func() (bool, error) {
		name := types.NamespacedName{Name: "version"}
		if err := kclient.Get(context.TODO(), name, &version); err != nil {
			t.Log(err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	desiredVersion := version.Status.Desired.Version
	if len(desiredVersion) == 0 && len(version.Status.History) > 0 {
		desiredVersion = version.Status.History[0].Version
	}

	var featureGate configv1.FeatureGate
	if err := wait.PollImmediate(1*time.Second, 1*time.Minute, func() (bool, error) {
		name := types.NamespacedName{Name: "cluster"}
		if err := kclient.Get(context.TODO(), name, &featureGate); err != nil {
			t.Log(err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	foundVersion := false
	var enabledFeatures []configv1.FeatureGateName
	for _, featureGateValues := range featureGate.Status.FeatureGates {
		if featureGateValues.Version != desiredVersion {
			continue
		}
		foundVersion = true
		for _, enabled := range featureGateValues.Enabled {
			enabledFeatures = append(enabledFeatures, enabled.Name)
		}
	}
	if !foundVersion {
		t.Fatalf("missing desired version %q in featuregates.config.openshift.io/cluster", desiredVersion)
	}
	return enabledFeatures
}

// TestGatewayAPIResources tests that basic functions for Gateway API Custom Resources are functional.
// It specifically verifies that when the GatewayAPI feature gate is enabled, then a user can
// create a GatewayClass, Gateway, and HTTPRoute.
func TestGatewayAPIResources(t *testing.T) {
	t.Parallel()

	// Create a test namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: names.SimpleNameGenerator.GenerateName("test-e2e-gwapi-"),
		},
	}
	if err := kclient.Create(context.TODO(), ns); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			t.Fatalf("error creating namespace: %v", err)
		}
	}
	defer func() {
		// Deleting the namespace also deletes the httproute
		if err := kclient.Delete(context.TODO(), ns); err != nil {
			t.Fatalf("failed to delete test namespace %s: %v", ns.Name, err)
		}
	}()

	// Check if the feature gate is disabled, and if it is, make sure no GWAPI CRDs can be created
	// Get the cluster feature gate
	enabledFeatures := getEnabledFeatures(t)
	gatewayAPIEnabled := false
	for _, feature := range enabledFeatures {
		if feature == configv1.FeatureGateGatewayAPI {
			gatewayAPIEnabled = true
		}
	}
	if !gatewayAPIEnabled {
		t.Logf("feature gate not enabled")

		// Make sure nothing can happen with the Gateway API CRDs when the feature is disabled
		ok, gatewayClass := assertCanCreateGatewayClass(t, ns.Name)
		if ok {
			t.Fatalf("feature gate was not enabled, but gateway class object could be created")
		}

		ok, gateway := assertCanCreateGateway(t, gatewayClass)
		if ok {
			t.Fatalf("feature gate was not enabled, but gateway object could be created")
		}

		ok = assertCanCreateHttpRoute(t, ns.Name, gateway)
		if ok {
			t.Fatalf("feature gate was not enabled, but http route object could be created")
		}

		// Enable the feature gate for the rest of the test
		var featureGate configv1.FeatureGate
		if err := wait.PollImmediate(1*time.Second, 1*time.Minute, func() (bool, error) {
			name := types.NamespacedName{Name: "cluster"}
			if err := kclient.Get(context.TODO(), name, &featureGate); err != nil {
				t.Log(err)
				return false, nil
			}
			return true, nil
		}); err != nil {
			t.Fatal(err)
		}
		featureGate.Spec.FeatureGateSelection.FeatureSet = configv1.CustomNoUpgrade
		featureGate.Spec.FeatureGateSelection.CustomNoUpgrade = &configv1.CustomFeatureGates{
			Enabled: append(enabledFeatures, configv1.FeatureGateGatewayAPI),
		}
		if err := wait.PollImmediate(1*time.Second, 1*time.Minute, func() (bool, error) {
			if err := kclient.Update(context.TODO(), &featureGate); err != nil {
				t.Log(err)
				return false, nil
			}
			return true, nil
		}); err != nil {
			t.Fatalf("error enabling feature gate: %v", err)
		}
		t.Logf("enabled feature gate")
	}

	// Make sure all the *.gateway.networking.k8s.io CRDs are available since FeatureGate is enabled
	for _, crdName := range crdNames {
		if err := assertCrdExists(t, crdName); err != nil {
			t.Fatalf("failed to find crd %s: %v", crdName, err)
		}
		t.Logf("found crd %v", crdName)
	}

	// Reinitialize the client cache (otherwise we can't work with CRD instances)
	kubeConfig, err := config.GetConfig()
	if err != nil {
		t.Fatalf("failed to get kube config: %s\n", err)
	}
	kclient, err = operatorclient.NewClient(kubeConfig)
	if err != nil {
		t.Fatalf("failed to create kube config: %s\n", err)
	}

	// Now make sure all the *.gateway.networking.k8s.io CRDs can be used
	ok, gatewayClass := assertCanCreateGatewayClass(t, ns.Name)
	if !ok {
		t.Fatalf("feature gate was enabled, but gateway class object could not be created")
	}
	// We don't need to delete the gateway class.

	ok, gateway := assertCanCreateGateway(t, gatewayClass)
	if !ok {
		t.Fatalf("feature gate was enabled, but gateway object could not be created")
	}
	defer func() {
		// Delete the gateway
		if gateway != nil {
			if err := kclient.Delete(context.TODO(), gateway); err != nil {
				t.Logf("failed to delete test gateway %s: %v", gateway.Name, err)
			}
		}
	}()

	ok = assertCanCreateHttpRoute(t, ns.Name, gateway)
	if !ok {
		t.Fatalf("feature gate was enabled, but http route object could not be created")
	}
	// We don't need to delete the http route, it is cleaned up when the namespace is deleted.
}

func assertCrdExists(t *testing.T, crdname string) error {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	name := types.NamespacedName{"", crdname}

	err := wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
		if err := kclient.Get(context.TODO(), name, crd); err != nil {
			t.Logf("failed to get crd %s: %v", name, err)
			return false, nil
		}
		crdConditions := crd.Status.Conditions
		for _, c := range crdConditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		t.Logf("failed to find crd %s to be Established", name)
		return false, nil
	})
	return err
}

func assertCanCreateGatewayClass(t *testing.T, ns string) (bool, *v1beta1.GatewayClass) {
	t.Helper()

	gatewayClass, err := createGatewayClass(t)
	if err != nil {
		t.Logf("error creating gateway class: %v", err)
		return false, nil
	}
	return true, gatewayClass
}

func assertCanCreateGateway(t *testing.T, gatewayClass *v1beta1.GatewayClass) (bool, *v1beta1.Gateway) {
	t.Helper()

	gateway, err := createGateway(gatewayClass)
	if err != nil {
		t.Logf("error creating gateway: %v", err)
		return false, nil
	}
	return true, gateway
}

func assertCanCreateHttpRoute(t *testing.T, ns string, gateway *v1beta1.Gateway) bool {
	t.Helper()

	err := createHttpRoute(ns, gateway)
	if err != nil {
		t.Logf("error creating httpRoute: %v", err)
		return false
	}
	return true
}

// Check if HTTPRoute can be created.
func createHttpRoute(ns string, gateway *v1beta1.Gateway) error {
	httpRoute := buildHTTPRoute("test-httproute", ns, gateway.Name, "openshift-ingress", "test-hostname.example.com", "test-app", 8080)
	if err := kclient.Create(context.TODO(), httpRoute); err != nil {
		return err
	}
	return nil
}

// Check if Gateway can be created.
func createGateway(gatewayClass *v1beta1.GatewayClass) (*v1beta1.Gateway, error) {
	gateway := buildGateway("test-gateway", "openshift-ingress", gatewayClass.Name)
	if err := kclient.Create(context.TODO(), gateway); err != nil {
		return nil, err
	}
	return gateway, nil
}

// Check if GatewayClass can be created.
func createGatewayClass(t *testing.T) (*v1beta1.GatewayClass, error) {
	t.Helper()

	gatewayClass := buildGatewayClass("openshift-default", "openshift.io/gateway-controller")
	if err := kclient.Create(context.TODO(), gatewayClass); err != nil {
		if kerrors.IsAlreadyExists(err) {
			name := types.NamespacedName{"", "openshift-default"}
			if err = kclient.Get(context.TODO(), name, gatewayClass); err == nil {
				t.Logf("gateway class already exists")
				return gatewayClass, nil
			}
		} else {
			return nil, errors.New("failed to create gateway class: " + err.Error())
		}
	}
	return gatewayClass, nil
}

func buildGatewayClass(name, controllerName string) *v1beta1.GatewayClass {
	return &v1beta1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1beta1.GatewayClassSpec{
			ControllerName: v1beta1.GatewayController(controllerName),
		},
	}
}

func buildGateway(name, namespace, gcname string) *v1beta1.Gateway {
	hostname := v1beta1.Hostname(testHostname)
	all := v1beta1.FromNamespaces("All")
	allowedRoutes := v1beta1.AllowedRoutes{Namespaces: &v1beta1.RouteNamespaces{From: &all}}
	listener1 := v1beta1.Listener{Name: "http", Hostname: &hostname, Port: 80, Protocol: "HTTP", AllowedRoutes: &allowedRoutes}
	return &v1beta1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1beta1.GatewaySpec{
			GatewayClassName: v1beta1.ObjectName(gcname),
			Listeners:        []v1beta1.Listener{listener1},
		},
	}
}

func buildHTTPRoute(routename, namespace, parentgateway, parentnamespace, hostname, backendrefname string, backendrefport int) *v1beta1.HTTPRoute {
	parentns := v1beta1.Namespace(parentnamespace)
	parent := v1beta1.ParentReference{Name: v1beta1.ObjectName(parentgateway), Namespace: &parentns}
	port := v1beta1.PortNumber(backendrefport)
	rule := v1beta1.HTTPRouteRule{
		BackendRefs: []v1beta1.HTTPBackendRef{{
			BackendRef: v1beta1.BackendRef{
				BackendObjectReference: v1beta1.BackendObjectReference{
					Name: v1beta1.ObjectName(backendrefname),
					Port: &port,
				},
			},
		}},
	}

	return &v1beta1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routename, Namespace: namespace},
		Spec: v1beta1.HTTPRouteSpec{
			CommonRouteSpec: v1beta1.CommonRouteSpec{ParentRefs: []v1beta1.ParentReference{parent}},
			Hostnames:       []v1beta1.Hostname{v1beta1.Hostname(hostname)},
			Rules:           []v1beta1.HTTPRouteRule{rule},
		},
	}
}
