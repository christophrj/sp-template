package runtime

import (
	"context"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func WatchProviderConfig(ctx context.Context, gvr schema.GroupVersionResource,
	platformCluster *clusters.Cluster, updateChannel chan event.GenericEvent) error {
	dynClient, _ := dynamic.NewForConfig(platformCluster.RESTConfig())
	resource := dynClient.Resource(gvr)
	listWatch := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return resource.List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return resource.Watch(ctx, options)
		},
	}
	informer := cache.NewSharedInformer(listWatch, &unstructured.Unstructured{}, 0)
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured).DeepCopy()
			updateChannel <- event.GenericEvent{Object: u}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			u := newObj.(*unstructured.Unstructured).DeepCopy()
			updateChannel <- event.GenericEvent{Object: u}
		},
		DeleteFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured).DeepCopy()
			updateChannel <- event.GenericEvent{Object: u}
		},
	}); err != nil {
		return err
	}
	go informer.Run(ctx.Done())
	return nil
}
