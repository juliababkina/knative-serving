/*
Copyright 2018 The Knative Authors.

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

package labeler

import (
	"context"
	"fmt"
	"testing"

	// Inject the fake informers that this controller needs.
	servingclient "knative.dev/serving/pkg/client/injection/client/fake"
	_ "knative.dev/serving/pkg/client/injection/informers/serving/v1/configuration/fake"
	_ "knative.dev/serving/pkg/client/injection/informers/serving/v1/revision/fake"
	_ "knative.dev/serving/pkg/client/injection/informers/serving/v1/route/fake"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgotesting "k8s.io/client-go/testing"
	routereconciler "knative.dev/serving/pkg/client/injection/reconciler/serving/v1/route"

	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	v1 "knative.dev/serving/pkg/apis/serving/v1"

	. "knative.dev/pkg/reconciler/testing"
	. "knative.dev/serving/pkg/reconciler/testing/v1"
	. "knative.dev/serving/pkg/testing/v1"
)

// This is heavily based on the way the OpenShift Ingress controller tests its reconciliation method.
func TestReconcile(t *testing.T) {
	now := metav1.Now()

	table := TableTest{{
		Name: "bad workqueue key",
		// Make sure Reconcile handles bad keys.
		Key: "too/many/parts",
	}, {
		Name: "key not found",
		// Make sure Reconcile handles good keys that don't exist.
		Key: "foo/not-found",
	}, {
		Name: "label runLatest configuration",
		Objects: []runtime.Object{
			simpleRunLatest("default", "first-reconcile", "the-config"),
			simpleConfig("default", "the-config"),
			rev("default", "the-config"),
		},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchAddFinalizerAction("default", "first-reconcile"),
			patchAddLabel("default", rev("default", "the-config").Name,
				"serving.knative.dev/route", "first-reconcile"),
			patchAddLabel("default", "the-config", "serving.knative.dev/route", "first-reconcile"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", "first-reconcile"),
		},
		Key: "default/first-reconcile",
	}, {
		Name: "steady state",
		Objects: []runtime.Object{
			simpleRunLatest("default", "steady-state", "the-config", WithRouteFinalizer),
			simpleConfig("default", "the-config",
				WithConfigLabel("serving.knative.dev/route", "steady-state")),
			rev("default", "the-config",
				WithRevisionLabel("serving.knative.dev/route", "steady-state")),
		},
		Key: "default/steady-state",
	}, {
		Name: "failure adding label (revision)",
		// Induce a failure during patching
		WantErr: true,
		WithReactors: []clientgotesting.ReactionFunc{
			InduceFailure("patch", "revisions"),
		},
		Objects: []runtime.Object{
			simpleRunLatest("default", "add-label-failure", "the-config", WithRouteFinalizer),
			simpleConfig("default", "the-config"),
			rev("default", "the-config"),
		},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchAddLabel("default", rev("default", "the-config").Name,
				"serving.knative.dev/route", "add-label-failure"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "InternalError",
				`failed to add route label to /, Kind= "the-config-dbnfd": inducing failure for patch revisions`),
		},
		Key: "default/add-label-failure",
	}, {
		Name: "failure adding label (configuration)",
		// Induce a failure during patching
		WantErr: true,
		WithReactors: []clientgotesting.ReactionFunc{
			InduceFailure("patch", "configurations"),
		},
		Objects: []runtime.Object{
			simpleRunLatest("default", "add-label-failure", "the-config", WithRouteFinalizer),
			simpleConfig("default", "the-config"),
			rev("default", "the-config",
				WithRevisionLabel("serving.knative.dev/route", "add-label-failure")),
		},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchAddLabel("default", "the-config", "serving.knative.dev/route", "add-label-failure"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "InternalError",
				`failed to add route label to /, Kind= "the-config": inducing failure for patch configurations`),
		},
		Key: "default/add-label-failure",
	}, {
		Name:    "label config with incorrect label",
		WantErr: true,
		Objects: []runtime.Object{
			simpleRunLatest("default", "the-route", "the-config", WithRouteFinalizer),
			simpleConfig("default", "the-config",
				WithConfigLabel("serving.knative.dev/route", "another-route")),
			rev("default", "the-config",
				WithRevisionLabel("serving.knative.dev/route", "another-route")),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "InternalError",
				`/, Kind= "the-config-dbnfd" is already in use by "another-route", and cannot be used by "the-route"`),
		},
		Key: "default/the-route",
	}, {
		Name: "change configurations",
		Objects: []runtime.Object{
			simpleRunLatest("default", "config-change", "new-config", WithRouteFinalizer),
			simpleConfig("default", "old-config",
				WithConfigLabel("serving.knative.dev/route", "config-change")),
			rev("default", "old-config",
				WithRevisionLabel("serving.knative.dev/route", "config-change")),
			simpleConfig("default", "new-config"),
			rev("default", "new-config"),
		},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchRemoveLabel("default", rev("default", "old-config").Name,
				"serving.knative.dev/route"),
			patchAddLabel("default", rev("default", "new-config").Name,
				"serving.knative.dev/route", "config-change"),
			patchRemoveLabel("default", "old-config", "serving.knative.dev/route"),
			patchAddLabel("default", "new-config", "serving.knative.dev/route", "config-change"),
		},
		Key: "default/config-change",
	}, {
		Name: "delete route",
		Objects: []runtime.Object{
			simpleRunLatest("default", "delete-route", "the-config", WithRouteFinalizer, WithRouteDeletionTimestamp(&now)),
			simpleConfig("default", "the-config",
				WithConfigLabel("serving.knative.dev/route", "delete-route")),
		},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchRemoveLabel("default", "the-config", "serving.knative.dev/route"),
			patchRemoveFinalizerAction("default", "delete-route"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "FinalizerUpdate", `Updated "delete-route" finalizers`),
		},
		Key: "default/delete-route",
	}, {
		Name: "failure while removing a cfg annotation should return an error",
		// Induce a failure during patching
		WantErr: true,
		WithReactors: []clientgotesting.ReactionFunc{
			InduceFailure("patch", "configurations"),
		},
		Objects: []runtime.Object{
			simpleRunLatest("default", "delete-label-failure", "new-config", WithRouteFinalizer),
			simpleConfig("default", "old-config",
				WithConfigLabel("serving.knative.dev/route", "delete-label-failure")),
			simpleConfig("default", "new-config",
				WithConfigLabel("serving.knative.dev/route", "delete-label-failure")),
			rev("default", "new-config",
				WithRevisionLabel("serving.knative.dev/route", "delete-label-failure")),
			rev("default", "old-config"),
		},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchRemoveLabel("default", "old-config", "serving.knative.dev/route"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "InternalError",
				`failed to remove route label to /, Kind= "old-config": inducing failure for patch configurations`),
		},
		Key: "default/delete-label-failure",
	}, {
		Name: "failure while removing a rev annotation should return an error",
		// Induce a failure during patching
		WantErr: true,
		WithReactors: []clientgotesting.ReactionFunc{
			InduceFailure("patch", "revisions"),
		},
		Objects: []runtime.Object{
			simpleRunLatest("default", "delete-label-failure", "new-config", WithRouteFinalizer),
			simpleConfig("default", "old-config",
				WithConfigLabel("serving.knative.dev/route", "delete-label-failure")),
			simpleConfig("default", "new-config",
				WithConfigLabel("serving.knative.dev/route", "delete-label-failure")),
			rev("default", "new-config",
				WithRevisionLabel("serving.knative.dev/route", "delete-label-failure")),
			rev("default", "old-config",
				WithRevisionLabel("serving.knative.dev/route", "delete-label-failure")),
		},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchRemoveLabel("default", rev("default", "old-config").Name,
				"serving.knative.dev/route"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "InternalError",
				`failed to remove route label to /, Kind= "old-config-dbnfd": inducing failure for patch revisions`),
		},
		Key: "default/delete-label-failure",
	}}

	table.Test(t, MakeFactory(func(ctx context.Context, listers *Listers, cmw configmap.Watcher) controller.Reconciler {
		r := &Reconciler{
			client:              servingclient.Get(ctx),
			configurationLister: listers.GetConfigurationLister(),
			revisionLister:      listers.GetRevisionLister(),
		}

		return routereconciler.NewReconciler(ctx, logging.FromContext(ctx), servingclient.Get(ctx),
			listers.GetRouteLister(), controller.GetEventRecorder(ctx), r)
	}))
}

func routeWithTraffic(namespace, name string, traffic v1.TrafficTarget, opts ...RouteOption) *v1.Route {
	return Route(namespace, name, append(opts, WithStatusTraffic(traffic))...)
}

func simpleRunLatest(namespace, name, config string, opts ...RouteOption) *v1.Route {
	return routeWithTraffic(namespace, name, v1.TrafficTarget{
		RevisionName: config + "-dbnfd",
		Percent:      ptr.Int64(100),
	}, opts...)
}

func simpleConfig(namespace, name string, opts ...ConfigOption) *v1.Configuration {
	cfg := &v1.Configuration{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       namespace,
			Name:            name,
			ResourceVersion: "v1",
		},
	}
	cfg.Status.InitializeConditions()
	cfg.Status.SetLatestCreatedRevisionName(name + "-dbnfd")
	cfg.Status.SetLatestReadyRevisionName(name + "-dbnfd")

	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

func rev(namespace, name string, opts ...RevisionOption) *v1.Revision {
	cfg := simpleConfig(namespace, name)
	rev := &v1.Revision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       namespace,
			Name:            cfg.Status.LatestCreatedRevisionName,
			ResourceVersion: "v1",
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(cfg)},
		},
	}

	for _, opt := range opts {
		opt(rev)
	}
	return rev
}

func patchRemoveLabel(namespace, name, key string) clientgotesting.PatchActionImpl {
	action := clientgotesting.PatchActionImpl{}
	action.Name = name
	action.Namespace = namespace

	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, key)

	action.Patch = []byte(patch)
	return action
}

func patchAddLabel(namespace, name, key, value string) clientgotesting.PatchActionImpl {
	action := clientgotesting.PatchActionImpl{}
	action.Name = name
	action.Namespace = namespace

	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, key, value)

	action.Patch = []byte(patch)
	return action
}

func patchAddFinalizerAction(namespace, name string) clientgotesting.PatchActionImpl {
	resource := v1.Resource("routes")
	finalizer := resource.String()

	action := clientgotesting.PatchActionImpl{}
	action.Name = name
	action.Namespace = namespace
	patch := fmt.Sprintf(`{"metadata":{"finalizers":[%q],"resourceVersion":""}}`, finalizer)
	action.Patch = []byte(patch)
	return action
}

func patchRemoveFinalizerAction(namespace, name string) clientgotesting.PatchActionImpl {
	action := clientgotesting.PatchActionImpl{}
	action.Name = name
	action.Namespace = namespace
	action.Patch = []byte(`{"metadata":{"finalizers":[],"resourceVersion":""}}`)
	return action
}

func TestNew(t *testing.T) {
	ctx, _ := SetupFakeContext(t)

	c := NewController(ctx, configmap.NewStaticWatcher())

	if c == nil {
		t.Fatal("Expected NewController to return a non-nil value")
	}
}
