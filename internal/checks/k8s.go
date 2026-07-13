package checks

import (
	"context"
	"fmt"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// endpointSliceServiceLabel is the standard label kube-controller-manager
// stamps on every EndpointSlice it creates for a Service (P2 contract:
// "label kubernetes.io/service-name=<service>").
const endpointSliceServiceLabel = "kubernetes.io/service-name"

// resyncFallback is the informer's periodic full-relist interval — the
// watch is the primary change-detection path; this bounds how stale the
// cache can get if a watch silently wedges without erroring (P2 contract /
// task spec: "watch with resync fallback to 15s relist").
const resyncFallback = 15 * time.Second

// K8sLister is the client-go-backed EndpointSliceLister: an in-cluster
// EndpointSlice informer covering every namespace this responder's
// ClusterRole can read (P2 contract deviation, pre-approved: namespace-
// scoping via a real ClusterRole would need one Role/RoleBinding per
// registered namespace kept in sync with the registry — deferred, tracked
// as documented hardening).
type K8sLister struct {
	informer cache.SharedIndexInformer
}

// NewK8sLister builds an in-cluster client-go clientset, starts an
// EndpointSlice informer across all namespaces, and blocks until its
// initial cache sync completes or ctx is cancelled.
func NewK8sLister(ctx context.Context) (*K8sLister, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("checks: in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("checks: build clientset: %w", err)
	}
	return newK8sListerForClientset(ctx, clientset)
}

// newK8sListerForClientset is the constructor body factored out so tests
// can pass a k8s.io/client-go/kubernetes/fake clientset instead of standing
// up a real (or envtest) cluster.
func newK8sListerForClientset(ctx context.Context, clientset kubernetes.Interface) (*K8sLister, error) {
	factory := informers.NewSharedInformerFactory(clientset, resyncFallback)
	informer := factory.Discovery().V1().EndpointSlices().Informer()

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return nil, fmt.Errorf("checks: EndpointSlice informer cache sync failed (context done)")
	}
	return &K8sLister{informer: informer}, nil
}

// ReadyCount implements EndpointSliceLister: the total number of ready
// endpoint addresses across every EndpointSlice for (namespace, service)
// currently in the informer's cache.
func (l *K8sLister) ReadyCount(namespace, service string) int {
	count := 0
	for _, obj := range l.informer.GetStore().List() {
		es, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			continue
		}
		if es.Namespace != namespace {
			continue
		}
		if es.Labels[endpointSliceServiceLabel] != service {
			continue
		}
		for _, ep := range es.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				count += len(ep.Addresses)
			}
		}
	}
	return count
}
