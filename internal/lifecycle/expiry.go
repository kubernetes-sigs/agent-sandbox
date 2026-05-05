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

package lifecycle

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FinishedCondition returns the terminal condition when it is present and true.
func FinishedCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	condition := meta.FindStatusCondition(conditions, conditionType)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return nil
	}
	return condition
}

// NeedsCleanup reports whether ttl-after-finished cleanup applies.
func NeedsCleanup(ttlSecondsAfterFinished *int32, finishedCondition *metav1.Condition) bool {
	return ttlSecondsAfterFinished != nil && finishedCondition != nil
}

// FinishedTime returns the finish timestamp encoded in the terminal condition.
func FinishedTime(finishedCondition *metav1.Condition) *time.Time {
	if finishedCondition == nil || finishedCondition.LastTransitionTime.IsZero() {
		return nil
	}
	finishedAt := finishedCondition.LastTransitionTime.Time
	return &finishedAt
}

// ExpireAt returns the earliest configured expiry time.
func ExpireAt(shutdownTime *metav1.Time, ttlSecondsAfterFinished *int32, finishedCondition *metav1.Condition) *time.Time {
	var expireAt *time.Time
	if shutdownTime != nil {
		shutdownAt := shutdownTime.Time
		expireAt = &shutdownAt
	}

	if !NeedsCleanup(ttlSecondsAfterFinished, finishedCondition) {
		return expireAt
	}

	finishedAt := FinishedTime(finishedCondition)
	if finishedAt == nil {
		return expireAt
	}

	ttlExpireAt := finishedAt.Add(time.Duration(*ttlSecondsAfterFinished) * time.Second)
	if expireAt == nil || ttlExpireAt.Before(*expireAt) {
		expireAt = &ttlExpireAt
	}

	return expireAt
}

// TimeLeft reports whether the resource has expired and, if not, how long remains.
func TimeLeft(now time.Time, shutdownTime *metav1.Time, ttlSecondsAfterFinished *int32, finishedCondition *metav1.Condition) (bool, time.Duration) {
	expireAt := ExpireAt(shutdownTime, ttlSecondsAfterFinished, finishedCondition)
	if expireAt == nil {
		return false, 0
	}
	if !now.Before(*expireAt) {
		return true, 0
	}
	return false, expireAt.Sub(now)
}
