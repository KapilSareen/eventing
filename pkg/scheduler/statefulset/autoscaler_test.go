/*
Copyright 2020 The Knative Authors

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

package statefulset

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/listers/core/v1"
	gtesting "k8s.io/client-go/testing"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/reconciler"

	kubeclient "knative.dev/pkg/client/injection/kube/client/fake"
	_ "knative.dev/pkg/client/injection/kube/informers/apps/v1/statefulset/fake"

	listers "knative.dev/eventing/pkg/reconciler/testing/v1"

	duckv1alpha1 "knative.dev/eventing/pkg/apis/duck/v1alpha1"
	"knative.dev/eventing/pkg/scheduler"
	"knative.dev/eventing/pkg/scheduler/state"
	tscheduler "knative.dev/eventing/pkg/scheduler/testing"
)

const (
	testNs = "test-ns"
)

func TestAutoscaler(t *testing.T) {
	testCases := []struct {
		name         string
		replicas     int32
		vpods        []scheduler.VPod
		scaleDown    bool
		wantReplicas int32
		reserved     map[types.NamespacedName]map[string]int32
	}{
		{
			name:     "no replicas, no placements, no pending",
			replicas: int32(0),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 0, nil),
			},
			wantReplicas: int32(0),
		},
		{
			name:     "no replicas, no placements, with pending",
			replicas: int32(0),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 5, nil),
			},
			wantReplicas: int32(1),
		},
		{
			name:     "no replicas, with placements, no pending",
			replicas: int32(0),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 15, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(2),
		},
		{
			name:     "no replicas, with placements, with pending, enough capacity",
			replicas: int32(0),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 18, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(2),
		},
		{
			name:     "no replicas, with placements, with pending, not enough capacity",
			replicas: int32(0),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 23, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(3),
		},
		{
			name:     "with replicas, no placements, no pending, scale down",
			replicas: int32(3),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 0, nil),
			},
			scaleDown:    true,
			wantReplicas: int32(0),
		},
		{
			name:         "with replicas, no placements, no pending, scale down (no vpods)",
			replicas:     int32(3),
			vpods:        []scheduler.VPod{},
			scaleDown:    true,
			wantReplicas: int32(0),
		},
		{
			name:     "with replicas, no placements, with pending, scale down",
			replicas: int32(3),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 5, nil),
			},
			scaleDown:    true,
			wantReplicas: int32(1),
		},
		{
			name:     "with replicas, no placements, with pending, scale down disabled",
			replicas: int32(3),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 5, nil),
			},
			scaleDown:    false,
			wantReplicas: int32(3),
		},
		{
			name:     "with replicas, no placements, with pending, scale up",
			replicas: int32(3),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 45, nil),
			},
			wantReplicas: int32(5),
		},
		{
			name:     "with replicas, no placements, with pending, no change",
			replicas: int32(3),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 25, nil),
			},
			wantReplicas: int32(3),
		},
		{
			name:     "with replicas, with placements, no pending, no change",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 15, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(2),
		},
		{
			name:     "with replicas, with placements, with reserved",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 12, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(5)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(2),
			reserved: map[types.NamespacedName]map[string]int32{
				{Namespace: testNs, Name: "vpod-1"}: {
					"statefulset-name-0": 8,
				},
			},
		},
		{
			name:     "with replicas, with placements, with reserved (scale up)",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 22, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(2)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(3),
			reserved: map[types.NamespacedName]map[string]int32{
				{Namespace: testNs, Name: "vpod-1"}: {
					"statefulset-name-0": 9,
				},
			},
		},
		{
			name:     "with replicas, with placements, with pending (scale up)",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 21, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(5)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(3),
		},
		{
			name:     "with replicas, with placements, with pending (scale up)",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 21, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(5)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
				tscheduler.NewVPod(testNs, "vpod-2", 19, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(5)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(4),
		},
		{
			name:     "with replicas, with placements, with pending (scale up), 1 over capacity",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 21, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(5)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
				tscheduler.NewVPod(testNs, "vpod-2", 20, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(5)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(5),
		},
		{
			name:     "with replicas, with placements, with pending, attempt scale down",
			replicas: int32(3),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 21, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(5)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(3),
			scaleDown:    true,
		},
		{
			name:     "with replicas, with placements, no pending, scale down",
			replicas: int32(5),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 15, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			scaleDown:    true,
			wantReplicas: int32(2),
		},
		{
			name:     "with replicas, with placements, with pending, enough capacity",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 18, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(2),
		},
		{
			name:     "with replicas, with placements, with pending, not enough capacity",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 23, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantReplicas: int32(3),
		},
		{
			name:     "with replicas, with placements, no pending, round up capacity",
			replicas: int32(5),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 20, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)},
					{PodName: "statefulset-name-2", VReplicas: int32(1)},
					{PodName: "statefulset-name-3", VReplicas: int32(1)},
					{PodName: "statefulset-name-4", VReplicas: int32(1)}}),
			},
			wantReplicas: int32(5),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := tscheduler.SetupFakeContext(t)

			podlist := make([]runtime.Object, 0, tc.replicas)
			vpodClient := tscheduler.NewVPodClient()

			for i := int32(0); i < int32(math.Max(float64(tc.wantReplicas), float64(tc.replicas))); i++ {
				nodeName := "node" + fmt.Sprint(i)
				podName := sfsName + "-" + fmt.Sprint(i)
				pod, err := kubeclient.Get(ctx).CoreV1().Pods(testNs).Create(ctx, tscheduler.MakePod(testNs, podName, nodeName), metav1.CreateOptions{})
				if err != nil {
					t.Fatal("unexpected error", err)
				}
				podlist = append(podlist, pod)
			}

			var lspp v1.PodNamespaceLister
			if len(podlist) != 0 {
				lsp := listers.NewListers(podlist)
				lspp = lsp.GetPodLister().Pods(testNs)
			}

			scaleCache := scheduler.NewScaleCache(ctx, testNs, kubeclient.Get(ctx).AppsV1().StatefulSets(testNs), scheduler.ScaleCacheConfig{RefreshPeriod: time.Minute * 5})

			stateAccessor := state.NewStateBuilder(sfsName, vpodClient.List, 10, lspp, scaleCache)

			sfsClient := kubeclient.Get(ctx).AppsV1().StatefulSets(testNs)
			_, err := sfsClient.Create(ctx, tscheduler.MakeStatefulset(testNs, sfsName, tc.replicas), metav1.CreateOptions{})
			if err != nil {
				t.Fatal("unexpected error", err)
			}

			noopEvictor := func(pod *corev1.Pod, vpod scheduler.VPod, from *duckv1alpha1.Placement) error {
				return nil
			}

			cfg := &Config{
				StatefulSetNamespace: testNs,
				StatefulSetName:      sfsName,
				VPodLister:           vpodClient.List,
				Evictor:              noopEvictor,
				RefreshPeriod:        10 * time.Second,
				PodCapacity:          10,
				getReserved: func() map[types.NamespacedName]map[string]int32 {
					return tc.reserved
				},
			}
			autoscaler := newAutoscaler(cfg, stateAccessor, scaleCache)
			_ = autoscaler.Promote(reconciler.UniversalBucket(), nil)

			for _, vpod := range tc.vpods {
				vpodClient.Append(vpod)
			}

			err = autoscaler.syncAutoscale(ctx, tc.scaleDown)
			if err != nil {
				t.Fatal("unexpected error", err)
			}

			scale, err := sfsClient.GetScale(ctx, sfsName, metav1.GetOptions{})
			if err != nil {
				t.Fatal("unexpected error", err)
			}
			if scale.Spec.Replicas != tc.wantReplicas {
				t.Errorf("unexpected number of replicas, got %d, want %d", scale.Spec.Replicas, tc.wantReplicas)
			}

		})
	}
}

func TestAutoscalerScaleDownToZero(t *testing.T) {
	ctx, cancel := tscheduler.SetupFakeContext(t)

	afterUpdate := make(chan bool)
	kubeclient.Get(ctx).PrependReactor("update", "statefulsets", func(action gtesting.Action) (handled bool, ret runtime.Object, err error) {
		if action.GetSubresource() == "scale" {
			afterUpdate <- true
		}
		return false, nil, nil
	})

	vpodClient := tscheduler.NewVPodClient()
	scaleCache := scheduler.NewScaleCache(ctx, testNs, kubeclient.Get(ctx).AppsV1().StatefulSets(testNs), scheduler.ScaleCacheConfig{RefreshPeriod: time.Minute * 5})
	stateAccessor := state.NewStateBuilder(sfsName, vpodClient.List, 10, nil, scaleCache)

	sfsClient := kubeclient.Get(ctx).AppsV1().StatefulSets(testNs)
	_, err := sfsClient.Create(ctx, tscheduler.MakeStatefulset(testNs, sfsName, 10), metav1.CreateOptions{})
	if err != nil {
		t.Fatal("unexpected error", err)
	}

	noopEvictor := func(pod *corev1.Pod, vpod scheduler.VPod, from *duckv1alpha1.Placement) error {
		return nil
	}

	cfg := &Config{
		StatefulSetNamespace: testNs,
		StatefulSetName:      sfsName,
		VPodLister:           vpodClient.List,
		Evictor:              noopEvictor,
		RefreshPeriod:        2 * time.Second,
		PodCapacity:          10,
		getReserved: func() map[types.NamespacedName]map[string]int32 {
			return nil
		},
	}
	autoscaler := newAutoscaler(cfg, stateAccessor, scaleCache)
	_ = autoscaler.Promote(reconciler.UniversalBucket(), nil)

	done := make(chan bool)
	go func() {
		autoscaler.Start(ctx)
		done <- true
	}()

	select {
	case <-afterUpdate:
	case <-time.After(4 * time.Second):
		t.Fatal("timeout waiting for scale subresource to be updated")

	}

	sfs, err := sfsClient.Get(ctx, sfsName, metav1.GetOptions{})
	if err != nil {
		t.Fatal("unexpected error", err)
	}
	if *sfs.Spec.Replicas != 0 {
		t.Errorf("unexpected number of replicas, got %d, want 3", *sfs.Spec.Replicas)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for autoscaler to stop")
	}
}

func TestCompactor(t *testing.T) {
	testCases := []struct {
		name          string
		replicas      int32
		vpods         []scheduler.VPod
		wantEvictions map[types.NamespacedName][]duckv1alpha1.Placement
	}{
		{
			name:     "no replicas, no placements, no pending",
			replicas: int32(0),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 0, nil),
			},
			wantEvictions: nil,
		},
		{
			name:     "one vpod, with placements in 2 pods, compacted",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 15, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
			},
			wantEvictions: nil,
		},
		{
			name:     "one vpod, with  placements in 2 pods, compacted edge",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 11, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(3)}}),
			},
			wantEvictions: nil,
		},
		{
			name:     "one vpod, with placements in 2 pods, not compacted",
			replicas: int32(2),
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 10, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(8)},
					{PodName: "statefulset-name-1", VReplicas: int32(2)}}),
			},
			wantEvictions: map[types.NamespacedName][]duckv1alpha1.Placement{
				{Name: "vpod-1", Namespace: testNs}: {{PodName: "statefulset-name-1", VReplicas: int32(2)}},
			},
		},
		{
			name:     "multiple vpods, with placements in multiple pods, compacted",
			replicas: int32(3),
			// pod-0:6, pod-1:8, pod-2:7
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 12, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(4)},
					{PodName: "statefulset-name-1", VReplicas: int32(8)}}),
				tscheduler.NewVPod(testNs, "vpod-2", 9, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(2)},
					{PodName: "statefulset-name-2", VReplicas: int32(7)}}),
			},
			wantEvictions: nil,
		},
		{
			name:     "multiple vpods, with placements in multiple pods, not compacted",
			replicas: int32(3),
			// pod-0:6, pod-1:7, pod-2:7
			vpods: []scheduler.VPod{
				tscheduler.NewVPod(testNs, "vpod-1", 6, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(4)},
					{PodName: "statefulset-name-1", VReplicas: int32(7)}}),
				tscheduler.NewVPod(testNs, "vpod-2", 15, []duckv1alpha1.Placement{
					{PodName: "statefulset-name-0", VReplicas: int32(2)},
					{PodName: "statefulset-name-2", VReplicas: int32(7)}}),
			},
			wantEvictions: map[types.NamespacedName][]duckv1alpha1.Placement{
				{Name: "vpod-2", Namespace: testNs}: {{PodName: "statefulset-name-2", VReplicas: int32(7)}},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := tscheduler.SetupFakeContext(t)

			podlist := make([]runtime.Object, 0, tc.replicas)
			vpodClient := tscheduler.NewVPodClient()

			for i := int32(0); i < tc.replicas; i++ {
				nodeName := "node" + fmt.Sprint(i)
				podName := sfsName + "-" + fmt.Sprint(i)
				pod, err := kubeclient.Get(ctx).CoreV1().Pods(testNs).Create(ctx, tscheduler.MakePod(testNs, podName, nodeName), metav1.CreateOptions{})
				if err != nil {
					t.Fatal("unexpected error", err)
				}
				podlist = append(podlist, pod)
			}

			_, err := kubeclient.Get(ctx).AppsV1().StatefulSets(testNs).Create(ctx, tscheduler.MakeStatefulset(testNs, sfsName, tc.replicas), metav1.CreateOptions{})
			if err != nil {
				t.Fatal("unexpected error", err)
			}

			lsp := listers.NewListers(podlist)
			scaleCache := scheduler.NewScaleCache(ctx, testNs, kubeclient.Get(ctx).AppsV1().StatefulSets(testNs), scheduler.ScaleCacheConfig{RefreshPeriod: time.Minute * 5})
			stateAccessor := state.NewStateBuilder(sfsName, vpodClient.List, 10, lsp.GetPodLister().Pods(testNs), scaleCache)

			evictions := make(map[types.NamespacedName][]duckv1alpha1.Placement)
			recordEviction := func(pod *corev1.Pod, vpod scheduler.VPod, from *duckv1alpha1.Placement) error {
				evictions[vpod.GetKey()] = append(evictions[vpod.GetKey()], *from)
				return nil
			}

			cfg := &Config{
				StatefulSetNamespace: testNs,
				StatefulSetName:      sfsName,
				VPodLister:           vpodClient.List,
				Evictor:              recordEviction,
				RefreshPeriod:        10 * time.Second,
				PodCapacity:          10,
			}
			autoscaler := newAutoscaler(cfg, stateAccessor, scaleCache)
			_ = autoscaler.Promote(reconciler.UniversalBucket(), func(bucket reconciler.Bucket, name types.NamespacedName) {})
			assert.Equal(t, true, autoscaler.isLeader.Load())

			for _, vpod := range tc.vpods {
				vpodClient.Append(vpod)
			}

			state, err := stateAccessor.State(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if err := autoscaler.mayCompact(logging.FromContext(ctx), state); err != nil {
				t.Fatal(err)
			}

			if tc.wantEvictions == nil && len(evictions) != 0 {
				t.Fatalf("unexpected evictions: %v", evictions)

			}
			for key, placements := range tc.wantEvictions {
				got, ok := evictions[key]
				if !ok {
					t.Fatalf("unexpected %v to be evicted but was not", key)
				}

				if !reflect.DeepEqual(placements, got) {
					t.Fatalf("expected evicted placement to be %v, but got %v", placements, got)
				}

				delete(evictions, key)
			}

			if len(evictions) != 0 {
				t.Fatalf("unexpected evictions %v", evictions)
			}

			autoscaler.Demote(reconciler.UniversalBucket())
			assert.Equal(t, false, autoscaler.isLeader.Load())
		})
	}
}

func TestEphemeralKeyStableValues(t *testing.T) {
	// Do not modify expected values
	assert.Equal(t, "knative-eventing", ephemeralLeaderElectionObject.Namespace)
	assert.Equal(t, "autoscaler-ephemeral", ephemeralLeaderElectionObject.Name)
}
