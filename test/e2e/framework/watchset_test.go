package framework

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestWatchSetRecreatesWatchAfterLastSubscriptionCloses(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxes"}
	scheme := runtime.NewScheme()
	dynClient := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		gvr: "SandboxList",
	})

	watchersStarted := make(chan *watch.FakeWatcher, 2)
	dynClient.PrependWatchReactor(gvr.Resource, func(_ ktesting.Action) (bool, watch.Interface, error) {
		fw := watch.NewFake()
		watchersStarted <- fw
		return true, fw, nil
	})

	ws := NewWatchSet(dynClient)
	defer ws.Close()

	filter := WatchFilter{Namespace: "default", Name: "sandbox-a"}

	firstSub := ws.Subscribe(gvr, filter)
	firstWatcher := waitForFakeWatcher(t, watchersStarted)
	firstSub.Close()
	waitForWatcherStopped(t, firstWatcher)

	secondSub := ws.Subscribe(gvr, filter)
	defer secondSub.Close()
	secondWatcher := waitForFakeWatcher(t, watchersStarted)

	if firstWatcher == secondWatcher {
		t.Fatalf("expected a new watcher after the last subscription closed")
	}
}

func waitForFakeWatcher(t *testing.T, started <-chan *watch.FakeWatcher) *watch.FakeWatcher {
	t.Helper()

	select {
	case fw := <-started:
		return fw
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch to start")
		return nil
	}
}

func waitForWatcherStopped(t *testing.T, fw *watch.FakeWatcher) {
	t.Helper()

	select {
	case _, ok := <-fw.ResultChan():
		if ok {
			t.Fatal("expected watcher result channel to be closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watcher to stop")
	}
}
