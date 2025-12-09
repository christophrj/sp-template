/*
Copyright 2025.

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

package controller

import (
	"context"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess"
	apiv1alpha1 "github.com/openmcp-project/service-provider-template/api/v1alpha1"
	spruntime "github.com/openmcp-project/service-provider-template/pkg/runtime"
	apiextensionclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// FooServiceReconciler reconciles a FooService object
type FooServiceReconciler struct {
	OnboardingCluster       *clusters.Cluster
	PlatformCluster         *clusters.Cluster
	ClusterAccessReconciler clusteraccess.Reconciler
}

// CreateOrUpdate is called on every add or update event
func (r *FooServiceReconciler) CreateOrUpdate(ctx context.Context, svcobj *apiv1alpha1.FooService, providerConfig *apiv1alpha1.ProviderConfig, target *clusters.Cluster) (ctrl.Result, error) {
	spruntime.StatusProgressing(svcobj, "Reconciling", "Reconcile in progress")
	l := logf.FromContext(ctx)
	managedObj := fooCRD()
	cl := apiextensionclient.NewForConfigOrDie(target.Cluster().GetConfig()).ApiextensionsV1()
	existing, err := cl.CustomResourceDefinitions().Get(ctx, managedObj.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		// create
		_, err := cl.CustomResourceDefinitions().Create(ctx, managedObj, metav1.CreateOptions{})
		if err != nil {
			l.Error(err, "create object failed")
			return ctrl.Result{}, err
		}
		spruntime.StatusReady(svcobj)
		return ctrl.Result{}, nil
	}
	if err != nil {
		l.Error(err, "get object failed")
		return ctrl.Result{}, err
	}
	// update
	managedObj.ResourceVersion = existing.ResourceVersion // required for update
	_, err = cl.CustomResourceDefinitions().Update(ctx, managedObj, metav1.UpdateOptions{})
	if err != nil {
		l.Error(err, "update object failed")
		return ctrl.Result{}, err
	}
	spruntime.StatusReady(svcobj)
	return ctrl.Result{}, nil
}

// Delete is called on every delete event
func (r *FooServiceReconciler) Delete(ctx context.Context, obj *apiv1alpha1.FooService, providerConfig *apiv1alpha1.ProviderConfig, target *clusters.Cluster) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	// remove managed objs
	spruntime.StatusTerminating(obj)
	managedObj := fooCRD()
	if err := target.Client().Delete(ctx, managedObj); client.IgnoreNotFound(err) != nil {
		l.Error(err, "delete object failed")
		return ctrl.Result{}, err
	}
	if err := target.Client().Get(ctx, client.ObjectKeyFromObject(managedObj), managedObj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// object still exists
	return ctrl.Result{
		RequeueAfter: time.Second * 10,
	}, nil
}

func fooCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foos.example.domain",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.domain",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"foo": {Type: "string"},
									},
								},
							},
						},
					},
				},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "foos",
				Singular: "foo",
				Kind:     "Foo",
				ListKind: "FooList",
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *FooServiceReconciler) SetupWithManager(mgr ctrl.Manager, providerConfigUpdates chan event.GenericEvent) error {
	spReconciler := spruntime.SPReconciler[*apiv1alpha1.FooService, *apiv1alpha1.ProviderConfig]{
		OnboardingCluster:       r.OnboardingCluster,
		PlatformCluster:         r.PlatformCluster,
		ClusterAccessReconciler: r.ClusterAccessReconciler,
		DomainServiceReconciler: r,
		EmptyAPIObj: func() *apiv1alpha1.FooService {
			return &apiv1alpha1.FooService{}
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1alpha1.FooService{}).
		// sets up reconciles whenever provider config controller sends update events
		WatchesRawSource(
			source.Channel(
				providerConfigUpdates,
				handler.EnqueueRequestsFromMapFunc(
					func(ctx context.Context, obj client.Object) []reconcile.Request {
						// update cached provider config
						if obj != nil {
							copy := obj.(*apiv1alpha1.ProviderConfig).DeepCopy()
							spReconciler.ProviderConfig.Store(&copy)
						} else {
							spReconciler.ProviderConfig.Store(nil)
						}
						// reconcile all existing objects
						var list apiv1alpha1.FooServiceList
						if err := r.OnboardingCluster.Client().List(ctx, &list); err != nil {
							return nil
						}
						reqs := make([]reconcile.Request, len(list.Items))
						for i := range list.Items {
							reqs[i] = reconcile.Request{
								NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
							}
						}
						return reqs
					},
				)),
		).
		Named("fooservice").
		Complete(&spReconciler)
}
