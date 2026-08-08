/*
Copyright 2026 The Swarmada Authors.

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

package cli

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ConfigFlags carries the connection flags shared by every command, resolved on
// the cobra root. They map to the same ~/.kube/config and KUBECONFIG that
// kubectl uses (RFC-0001 §9.5.1): swarmctl has no credentials of its own.
type ConfigFlags struct {
	// Kubeconfig is an explicit --kubeconfig path; empty means fall back to the
	// KUBECONFIG env var, then ~/.kube/config, then in-cluster config.
	Kubeconfig string
	// Context selects a named context from the kubeconfig; empty uses the
	// current-context.
	Context string
	// Namespace is the --namespace/-n override; empty means "use the namespace
	// the selected context is bound to".
	Namespace string
}

// Factory turns ConfigFlags into live clients. It is the single place that
// builds a rest.Config so every command inherits the same kubeconfig/context
// resolution and scheme registration.
type Factory struct {
	flags        ConfigFlags
	clientConfig clientcmd.ClientConfig
	scheme       *runtime.Scheme
}

// NewFactory wires the loading rules for flags. It does not touch the cluster —
// the kubeconfig is read lazily on the first client build, matching kubectl, so
// commands like `swarmctl version` work with no cluster reachable.
func NewFactory(flags ConfigFlags) *Factory {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if flags.Kubeconfig != "" {
		loadingRules.ExplicitPath = flags.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if flags.Context != "" {
		overrides.CurrentContext = flags.Context
	}

	scheme := runtime.NewScheme()
	// Both schemes: fleetv1 for the CRDs, client-go's for core types
	// (Events, SubjectAccessReview) used by the lifecycle/audit verbs.
	utilRuntimeMust(clientgoscheme.AddToScheme(scheme))
	utilRuntimeMust(fleetv1.AddToScheme(scheme))

	return &Factory{
		flags:        flags,
		clientConfig: clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides),
		scheme:       scheme,
	}
}

// Scheme exposes the registered scheme so callers can set a GVK on an object
// before marshaling it for -o yaml/json.
func (f *Factory) Scheme() *runtime.Scheme { return f.scheme }

// RESTConfig resolves the *rest.Config for the selected context.
func (f *Factory) RESTConfig() (*rest.Config, error) {
	cfg, err := f.clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return cfg, nil
}

// Namespace resolves the effective namespace: the --namespace flag wins,
// otherwise the namespace the selected context is bound to (defaulting to
// "default" when the context sets none), matching kubectl.
func (f *Factory) Namespace() (string, error) {
	if f.flags.Namespace != "" {
		return f.flags.Namespace, nil
	}
	ns, _, err := f.clientConfig.Namespace()
	if err != nil {
		return "", fmt.Errorf("resolving namespace: %w", err)
	}
	return ns, nil
}

// Client builds a controller-runtime client bound to the fleetv1 + core scheme.
// It is the typed client used for get/describe/apply and for the underlying
// reads/writes of the lifecycle verbs.
func (f *Factory) Client() (client.Client, error) {
	cfg, err := f.RESTConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: f.scheme})
	if err != nil {
		return nil, fmt.Errorf("building client: %w", err)
	}
	return c, nil
}

// Clientset builds a typed client-go Clientset, used for the SelfSubjectAccessReview
// custom-verb gate and for emitting namespace Events (e.g. a rejection reason).
func (f *Factory) Clientset() (kubernetes.Interface, error) {
	cfg, err := f.RESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	return cs, nil
}

func utilRuntimeMust(err error) {
	if err != nil {
		panic(err)
	}
}
