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

// EventFilter provides optional predicates to filter informer events
// before auto-register processes them. If a predicate returns false,
// the event is skipped entirely (the handler logic does not run).
// Nil functions are treated as "allow all" — only set the ones you
// need to filter.
type EventFilter struct {
	// Add is called on informer Add events. Return false to skip.
	Add func(obj client.Object) bool

	// Update is called on informer Update events. Return false to skip.
	// Both the old and new object are provided for comparison
	// (e.g. generation change, annotation diff).
	Update func(oldObj, newObj client.Object) bool

	// Delete is called on informer Delete events. Return false to skip.
	Delete func(obj client.Object) bool
}

// autoRegisterConfig holds the configuration for automatic resource
// registration via cache informer events.
type autoRegisterConfig struct {
	cache     cache.Cache
	obj       client.Object
	extractor ConfigExtractorFn
	filter    *EventFilter
}

// setupAutoRegister hooks into the cache informer for the configured
// object type and registers/unregisters resources automatically.
func setupAutoRegister(ctx context.Context, w *ExternalWatcher) error {
	informer, err := w.autoRegister.cache.GetInformer(ctx, w.autoRegister.obj)
	if err != nil {
		return fmt.Errorf("auto-register: failed to get informer: %w", err)
	}

	filter := w.autoRegister.filter

	_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cObj, ok := obj.(client.Object)
			if !ok {
				return
			}
			if filter != nil && filter.Add != nil && !filter.Add(cObj) {
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
			cOld, okOld := oldObj.(client.Object)
			cNew, okNew := newObj.(client.Object)
			if !okOld || !okNew {
				return
			}
			if filter != nil && filter.Update != nil && !filter.Update(cOld, cNew) {
				return
			}
			key := types.NamespacedName{Name: cNew.GetName(), Namespace: cNew.GetNamespace()}
			if !w.fetcher.IsResourceReadyToWatch(ctx, key) {
				if w.IsRegistered(key) {
					w.Unregister(key)
					w.logger.V(1).Info("auto-unregistered resource (no longer ready)", "resource", key.String())
				} else {
					w.logger.V(2).Info("auto-register: resource not ready, skipping", "resource", key.String())
				}
				return
			}
			config := w.autoRegister.extractor(cNew)
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
			if filter != nil && filter.Delete != nil && !filter.Delete(cObj) {
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
