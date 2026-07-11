package meshstate

import (
	"fmt"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// Controller attaches the managed projection to the already-existing
// EdgeProxy Service and Endpoints informers. It creates no additional watches.
type Controller struct {
	store *Store
	once  sync.Once
}

func NewController(store *Store) *Controller {
	return &Controller{store: store}
}

func (c *Controller) Register(serviceInformer, endpointsInformer cache.SharedIndexInformer, stopCh <-chan struct{}) error {
	var registerErr error
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
			} else {
				c.store.MarkSourceDegraded("managed Service/Endpoints cache stopped before initial sync")
			}
		}()
	})
	return registerErr
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
