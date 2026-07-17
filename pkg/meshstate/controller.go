package meshstate

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	managedRepairInterval = 15 * time.Second
	managedRepairTimeout  = 10 * time.Second
)

type managedObjectGetter interface {
	GetService(ctx context.Context, namespace, name string) (*v1.Service, error)
	GetEndpoints(ctx context.Context, namespace, name string) (*v1.Endpoints, error)
}

type kubeManagedObjectGetter struct {
	client clientset.Interface
}

func (g kubeManagedObjectGetter) GetService(ctx context.Context, namespace, name string) (*v1.Service, error) {
	return g.client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (g kubeManagedObjectGetter) GetEndpoints(ctx context.Context, namespace, name string) (*v1.Endpoints, error) {
	return g.client.CoreV1().Endpoints(namespace).Get(ctx, name, metav1.GetOptions{})
}

// Controller attaches the managed projection to the already-existing
// EdgeProxy Service and Endpoints informers. It creates no additional watches.
type Controller struct {
	store *Store
	once  sync.Once
}

func NewController(store *Store) *Controller {
	return &Controller{store: store}
}

func (c *Controller) Register(client clientset.Interface, serviceInformer, endpointsInformer cache.SharedIndexInformer, stopCh <-chan struct{}) error {
	if client == nil {
		return fmt.Errorf("managed mesh repair client is required")
	}
	repairClient := kubeManagedObjectGetter{client: client}
	c.once.Do(func() {
		serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { c.onService(obj) },
			UpdateFunc: func(_, obj interface{}) { c.onService(obj) },
			DeleteFunc: c.onServiceDelete,
		})
		endpointsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { c.onEndpoints(obj) },
			UpdateFunc: func(_, obj interface{}) { c.onEndpoints(obj) },
			DeleteFunc: c.onEndpointsDelete,
		})
		go func() {
			if cache.WaitForNamedCacheSync("managed runtime meshstate", stopCh, serviceInformer.HasSynced, endpointsInformer.HasSynced) {
				c.store.MarkSourceSynced()
				wait.Until(func() { c.repairManagedObjects(repairClient) }, managedRepairInterval, stopCh)
			} else {
				c.store.MarkSourceDegraded("managed Service/Endpoints cache stopped before initial sync")
			}
		}()
	})
	return nil
}

func (c *Controller) repairManagedObjects(client managedObjectGetter) {
	for _, key := range c.store.repairCandidates() {
		ctx, cancel := context.WithTimeout(context.Background(), managedRepairTimeout)
		service, serviceErr := client.GetService(ctx, key.Namespace, key.Name)
		cancel()
		if serviceErr != nil {
			klog.V(2).InfoS("Managed Service repair lookup failed", "service", key.String(), "err", serviceErr)
			continue
		}
		if service == nil {
			klog.ErrorS(fmt.Errorf("empty Service response"), "Managed Service repair lookup failed", "service", key.String())
			continue
		}

		ctx, cancel = context.WithTimeout(context.Background(), managedRepairTimeout)
		endpoints, endpointsErr := client.GetEndpoints(ctx, key.Namespace, key.Name)
		cancel()
		if endpointsErr != nil {
			klog.V(2).InfoS("Managed Endpoints repair lookup failed", "endpoints", key.String(), "err", endpointsErr)
			continue
		}
		if endpoints == nil {
			klog.ErrorS(fmt.Errorf("empty Endpoints response"), "Managed Endpoints repair lookup failed", "endpoints", key.String())
			continue
		}

		if err := c.store.UpsertService(service); err != nil {
			klog.ErrorS(err, "Repair managed Service projection", "service", key.String())
			continue
		}
		if err := c.store.UpsertEndpoints(endpoints); err != nil {
			klog.ErrorS(err, "Repair managed Endpoints projection", "endpoints", key.String())
			continue
		}
		if c.store.projectionVerified(key) {
			klog.InfoS("Recovered managed projection through MetaServer point lookup",
				"service", key.String(),
				"serviceResourceVersion", service.ResourceVersion,
				"endpointsResourceVersion", endpoints.ResourceVersion)
		} else {
			klog.V(2).InfoS("Managed projection remains invalid after MetaServer point lookup", "service", key.String())
		}
	}
}

func (c *Controller) onService(obj interface{}) {
	service, ok := obj.(*v1.Service)
	if !ok {
		klog.ErrorS(fmt.Errorf("unexpected object %T", obj), "managed mesh Service event")
		return
	}
	if err := c.store.UpsertService(service); err != nil {
		klog.ErrorS(err, "Reconcile managed Service", "namespace", service.Namespace, "name", service.Name)
	}
}

func (c *Controller) onEndpoints(obj interface{}) {
	endpoints, ok := obj.(*v1.Endpoints)
	if !ok {
		klog.ErrorS(fmt.Errorf("unexpected object %T", obj), "managed mesh Endpoints event")
		return
	}
	if err := c.store.UpsertEndpoints(endpoints); err != nil {
		klog.ErrorS(err, "Reconcile managed Endpoints", "namespace", endpoints.Namespace, "name", endpoints.Name)
	}
}

func (c *Controller) onServiceDelete(obj interface{}) {
	service, ok := deletedService(obj)
	if !ok {
		klog.ErrorS(fmt.Errorf("unexpected object %T", obj), "managed mesh Service delete")
		return
	}
	if err := c.store.DeleteService(service); err != nil {
		klog.ErrorS(err, "Delete managed Service projection", "namespace", service.Namespace, "name", service.Name)
	}
}

func (c *Controller) onEndpointsDelete(obj interface{}) {
	endpoints, ok := deletedEndpoints(obj)
	if !ok {
		klog.ErrorS(fmt.Errorf("unexpected object %T", obj), "managed mesh Endpoints delete")
		return
	}
	if err := c.store.DeleteEndpoints(endpoints); err != nil {
		klog.ErrorS(err, "Delete managed Endpoints projection", "namespace", endpoints.Namespace, "name", endpoints.Name)
	}
}

func deletedService(obj interface{}) (*v1.Service, bool) {
	if service, ok := obj.(*v1.Service); ok {
		return service, true
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, false
	}
	service, ok := tombstone.Obj.(*v1.Service)
	return service, ok
}

func deletedEndpoints(obj interface{}) (*v1.Endpoints, bool) {
	if endpoints, ok := obj.(*v1.Endpoints); ok {
		return endpoints, true
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, false
	}
	endpoints, ok := tombstone.Obj.(*v1.Endpoints)
	return endpoints, ok
}
