// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPodQueue(t *testing.T) {
	pq := NewPodQueue()
	hash := "test-hash"

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "pod-1",
			UID:               types.UID("uid-1"),
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "pod-2",
			UID:               types.UID("uid-2"),
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
	}

	// Test Add
	pq.Add(hash, pod1)
	pq.Add(hash, pod2)

	// Test Get (should get pod1 as it's older/better)
	gotPod, ok := pq.Get(hash)
	if !ok || gotPod == nil {
		t.Fatalf("expected to get a pod, got nil")
	}
	if gotPod.Name != "pod-1" {
		t.Errorf("expected pod-1, got %s", gotPod.Name)
	}

	// Test that it's now reserved, adding it back shouldn't make it available
	pq.Add(hash, pod1)
	gotPod2, ok := pq.Get(hash)
	if !ok || gotPod2 == nil {
		t.Fatalf("expected to get second pod, got nil")
	}
	if gotPod2.Name != "pod-2" {
		t.Errorf("expected pod-2, got %s", gotPod2.Name)
	}

	// Now queue should be empty
	_, ok = pq.Get(hash)
	if ok {
		t.Errorf("expected queue to be empty")
	}

	// Return pod1 to queue
	pq.Unreserve(hash, pod1)
	gotPod3, ok := pq.Get(hash)
	if !ok || gotPod3 == nil {
		t.Fatalf("expected to get pod back, got nil")
	}
	if gotPod3.Name != "pod-1" {
		t.Errorf("expected pod-1, got %s", gotPod3.Name)
	}

	// Complete adoption
	pq.Done(pod1)
	pq.Unreserve(hash, pod1) // this should do nothing because it's no longer reserved

	_, ok = pq.Get(hash)
	if ok {
		t.Errorf("expected queue to still be empty because pod was Done")
	}
}

func TestPodQueueConcurrency(t *testing.T) {
	pq := NewPodQueue()
	hash := "concurrent-hash"

	pods := make([]*corev1.Pod, 100)
	for i := 0; i < 100; i++ {
		pods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-" + string(rune(i)),
				UID:  types.UID("uid-" + string(rune(i))),
			},
		}
	}

	var wg sync.WaitGroup

	// Start adding
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, p := range pods {
			pq.Add(hash, p)
			time.Sleep(time.Millisecond)
		}
	}()

	// Start getting
	wg.Add(1)
	go func() {
		defer wg.Done()
		gotCount := 0
		for gotCount < 100 {
			p, ok := pq.Get(hash)
			if ok && p != nil {
				gotCount++
				pq.Done(p)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
}
