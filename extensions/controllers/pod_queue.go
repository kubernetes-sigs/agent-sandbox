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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubectl/pkg/util/podutils"
)

// PodQueue manages a pool of available pods, allowing atomic reservation
// and ensuring that multiple claims do not clash over the same pod.
type PodQueue struct {
	mu sync.Mutex

	// availableReady maps template hash -> map[pod UID]*corev1.Pod
	availableReady map[string]map[types.UID]*corev1.Pod

	// availableNotReady maps template hash -> map[pod UID]*corev1.Pod
	availableNotReady map[string]map[types.UID]*corev1.Pod

	// reserved maps pod UID to reservation timestamp
	reserved map[types.UID]time.Time
}

// NewPodQueue creates and initializes a new PodQueue.
func NewPodQueue() *PodQueue {
	return &PodQueue{
		availableReady:    make(map[string]map[types.UID]*corev1.Pod),
		availableNotReady: make(map[string]map[types.UID]*corev1.Pod),
		reserved:          make(map[types.UID]time.Time),
	}
}

// Add adds or updates a pod in the queue.
func (pq *PodQueue) Add(hash string, pod *corev1.Pod) {
	isReady := podutils.IsPodReady(pod)

	pq.mu.Lock()
	defer pq.mu.Unlock()

	// If the pod is already reserved, we don't add it back to the available pool
	// until explicitly un-reserved.
	if _, isReserved := pq.reserved[pod.UID]; isReserved {
		return
	}

	if isReady {
		// Remove from NotReady map if moving
		if pods, ok := pq.availableNotReady[hash]; ok {
			delete(pods, pod.UID)
		}
		if pq.availableReady[hash] == nil {
			pq.availableReady[hash] = make(map[types.UID]*corev1.Pod)
		}
		pq.availableReady[hash][pod.UID] = pod
	} else {
		// Remove from Ready map if it became unready
		if pods, ok := pq.availableReady[hash]; ok {
			delete(pods, pod.UID)
		}
		if pq.availableNotReady[hash] == nil {
			pq.availableNotReady[hash] = make(map[types.UID]*corev1.Pod)
		}
		pq.availableNotReady[hash][pod.UID] = pod
	}
}

// Remove removes a pod from the queue.
func (pq *PodQueue) Remove(hash string, pod *corev1.Pod) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	if pods, ok := pq.availableReady[hash]; ok {
		delete(pods, pod.UID)
		if len(pods) == 0 {
			delete(pq.availableReady, hash)
		}
	}
	if pods, ok := pq.availableNotReady[hash]; ok {
		delete(pods, pod.UID)
		if len(pods) == 0 {
			delete(pq.availableNotReady, hash)
		}
	}
	// Note: We don't remove from reserved here, waiting for Done().
}

// Get atomically selects an available pod for a given template hash and marks it as reserved.
// Prioritizes a Ready pod if available, otherwise picks a NotReady pod. Returns true if pod found.
func (pq *PodQueue) Get(hash string) (*corev1.Pod, bool) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	var podToPick *corev1.Pod

	// 1. Try to pick from availableReady
	if pods, ok := pq.availableReady[hash]; ok && len(pods) > 0 {
		for _, pod := range pods {
			podToPick = pod
			break // pick any
		}
		delete(pods, podToPick.UID)
		if len(pods) == 0 {
			delete(pq.availableReady, hash)
		}
	} else if pods, ok := pq.availableNotReady[hash]; ok && len(pods) > 0 {
		// 2. Fall back to availableNotReady
		for _, pod := range pods {
			podToPick = pod
			break // pick any
		}
		delete(pods, podToPick.UID)
		if len(pods) == 0 {
			delete(pq.availableNotReady, hash)
		}
	}

	if podToPick == nil {
		return nil, false
	}

	// Mark as reserved
	pq.reserved[podToPick.UID] = time.Now()

	return podToPick, true
}

// Done marks a pod as completely processed or adopted, permanently removing its reservation.
func (pq *PodQueue) Done(pod *corev1.Pod) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	delete(pq.reserved, pod.UID)
}

// Unreserve returns a pod to the available queue (e.g., if adoption failed).
func (pq *PodQueue) Unreserve(hash string, pod *corev1.Pod) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Only unreserve if it was actually reserved
	if _, ok := pq.reserved[pod.UID]; ok {
		delete(pq.reserved, pod.UID)

		isReady := podutils.IsPodReady(pod)
		if isReady {
			if pq.availableReady[hash] == nil {
				pq.availableReady[hash] = make(map[types.UID]*corev1.Pod)
			}
			pq.availableReady[hash][pod.UID] = pod
		} else {
			if pq.availableNotReady[hash] == nil {
				pq.availableNotReady[hash] = make(map[types.UID]*corev1.Pod)
			}
			pq.availableNotReady[hash][pod.UID] = pod
		}
	}
}
