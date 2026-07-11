package meshstate

import (
	"k8s.io/client-go/tools/cache"

	"github.com/kubeedge/edgemesh/pkg/apis/config/v1alpha1"
)

// Runtime owns the managed projection, its shared-informer adapter and the
// loopback-only status API. A nil Runtime means legacy-only operation.
type Runtime struct {
	Config     *v1alpha1.ManagedRuntimeConfig
	Store      *Store
	controller *Controller
	status     *StatusServer
}

func NewRuntime(config *v1alpha1.ManagedRuntimeConfig) (*Runtime, error) {
	if config == nil || !config.Enable {
		return nil, nil
	}
	store := NewStore()
	return &Runtime{
		Config:     config,
		Store:      store,
		controller: NewController(store),
		status:     NewStatusServer(DefaultStatusListenAddress, store),
	}, nil
}

func (r *Runtime) StartStatusServer() error {
	if r != nil {
		return r.status.Start()
	}
	return nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	return r.status.Close()
}

func (r *Runtime) RegisterInformers(serviceInformer, endpointsInformer cache.SharedIndexInformer, stopCh <-chan struct{}) error {
	if r == nil {
		return nil
	}
	return r.controller.Register(serviceInformer, endpointsInformer, stopCh)
}
