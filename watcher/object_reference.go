package watcher

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// objectReference is a minimal client.Object implementation used to create
// GenericEvent instances. It only carries name and namespace — just enough
// for handler.EnqueueRequestForObject to produce a reconcile.Request.
type objectReference struct {
	metav1.ObjectMeta `json:"metadata,omitzero"`
}

var _ runtime.Object = (*objectReference)(nil)

func (o *objectReference) GetObjectKind() schema.ObjectKind {
	return schema.EmptyObjectKind
}

func (o *objectReference) DeepCopyObject() runtime.Object {
	cp := *o
	return &cp
}
