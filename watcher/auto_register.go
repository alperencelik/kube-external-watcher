package watcher

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ConfigExtractorFn extracts a ResourceConfig from a Kubernetes object.
// Used with WithAutoRegister to automatically register resources when
// they are created or updated, and unregister them when deleted.
//
// Readiness checking is handled separately by the fetcher's
// IsResourceReadyToWatch method — see ResourceStateFetcher.
type ConfigExtractorFn func(obj client.Object) ResourceConfig

// autoRegisterConfig holds the configuration for automatic resource
// registration via cache informer events.
type autoRegisterConfig struct {
	cache     cache.Cache
	obj       client.Object
	extractor ConfigExtractorFn
}

// setupAutoRegister hooks into the cache informer for the configured
// object type and registers/unregisters resources automatically.
func setupAutoRegister(ctx context.Context, w *ExternalWatcher) error {
	informer, err := w.autoRegister.cache.GetInformer(ctx, w.autoRegister.obj)
	if err != nil {
		return fmt.Errorf("auto-register: failed to get informer: %w", err)
	}

	_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cObj, ok := obj.(client.Object)
			if !ok {
				return
			}
			key := types.NamespacedName{Name: cObj.GetName(), Namespace: cObj.GetNamespace()}
			if !w.fetcher.IsResourceReadyToWatch(ctx, key) {
				w.logger.V(2).Info("auto-register: resource not ready, skipping", "resource", key.String())
				return
			}
			config := w.autoRegister.extractor(cObj)
			w.Register(key, config)
			w.logger.V(1).Info("auto-registered resource", "resource", key.String())
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			cObj, ok := newObj.(client.Object)
			if !ok {
				return
			}
			key := types.NamespacedName{Name: cObj.GetName(), Namespace: cObj.GetNamespace()}
			if !w.fetcher.IsResourceReadyToWatch(ctx, key) {
				if w.IsRegistered(key) {
					w.Unregister(key)
					w.logger.V(1).Info("auto-unregistered resource (no longer ready)", "resource", key.String())
				} else {
					w.logger.V(2).Info("auto-register: resource not ready, skipping", "resource", key.String())
				}
				return
			}
			config := w.autoRegister.extractor(cObj)
			w.Register(key, config)
			w.logger.V(2).Info("auto-register updated resource config", "resource", key.String())
		},
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			cObj, ok := obj.(client.Object)
			if !ok {
				return
			}
			key := types.NamespacedName{Name: cObj.GetName(), Namespace: cObj.GetNamespace()}
			w.Unregister(key)
			w.logger.V(1).Info("auto-unregistered resource", "resource", key.String())
		},
	})
	if err != nil {
		return fmt.Errorf("auto-register: failed to add event handler: %w", err)
	}

	w.logger.Info("auto-register enabled for informer")
	return nil
}
