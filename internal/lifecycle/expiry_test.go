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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTimeLeft(t *testing.T) {
	now := time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC)
	finishedCondition := &metav1.Condition{
		Type:               "Finished",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(now.Add(-30 * time.Second)),
	}
	shutdownInFiveSeconds := metav1.NewTime(now.Add(5 * time.Second))
	shutdownInFiveMinutes := metav1.NewTime(now.Add(5 * time.Minute))
	zero := int32(0)
	twoMinutes := int32(120)

	testCases := []struct {
		name                    string
		shutdownTime            *metav1.Time
		ttlSecondsAfterFinished *int32
		finishedCondition       *metav1.Condition
		wantExpired             bool
		wantRequeue             time.Duration
	}{
		{
			name:                    "no expiry configured",
			shutdownTime:            nil,
			ttlSecondsAfterFinished: nil,
			finishedCondition:       nil,
			wantExpired:             false,
			wantRequeue:             0,
		},
		{
			name:                    "ttl only not yet expired",
			shutdownTime:            nil,
			ttlSecondsAfterFinished: &twoMinutes,
			finishedCondition:       finishedCondition,
			wantExpired:             false,
			wantRequeue:             90 * time.Second,
		},
		{
			name:                    "ttl zero expires immediately",
			shutdownTime:            nil,
			ttlSecondsAfterFinished: &zero,
			finishedCondition:       finishedCondition,
			wantExpired:             true,
			wantRequeue:             0,
		},
		{
			name:                    "earlier shutdown time wins",
			shutdownTime:            &shutdownInFiveSeconds,
			ttlSecondsAfterFinished: &twoMinutes,
			finishedCondition:       finishedCondition,
			wantExpired:             false,
			wantRequeue:             5 * time.Second,
		},
		{
			name:                    "later shutdown time loses to ttl",
			shutdownTime:            &shutdownInFiveMinutes,
			ttlSecondsAfterFinished: &twoMinutes,
			finishedCondition:       finishedCondition,
			wantExpired:             false,
			wantRequeue:             90 * time.Second,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			expired, requeueAfter := TimeLeft(now, tc.shutdownTime, tc.ttlSecondsAfterFinished, tc.finishedCondition)
			require.Equal(t, tc.wantExpired, expired)
			require.Equal(t, tc.wantRequeue, requeueAfter)
		})
	}
}
