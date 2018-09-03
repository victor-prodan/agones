// Copyright 2018 Google Inc. All Rights Reserved.
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

package gameservers

import (
	"net/http"
	"sync"
	"testing"

	"time"

	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/sdk"
	agtesting "agones.dev/agones/pkg/testing"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func TestSidecarRun(t *testing.T) {
	t.Parallel()

	fixtures := map[string]struct {
		state      v1alpha1.State
		f          func(*SDKServer, context.Context)
		recordings []string
	}{
		"ready": {
			state: v1alpha1.RequestReady,
			f: func(sc *SDKServer, ctx context.Context) {
				sc.Ready(ctx, &sdk.Empty{}) // nolint: errcheck
			},
		},
		"shutdown": {
			state: v1alpha1.Shutdown,
			f: func(sc *SDKServer, ctx context.Context) {
				sc.Shutdown(ctx, &sdk.Empty{}) // nolint: errcheck
			},
		},
		"unhealthy": {
			state: v1alpha1.Unhealthy,
			f: func(sc *SDKServer, ctx context.Context) {
				// we have a 1 second timeout
				time.Sleep(2 * time.Second)
			},
			recordings: []string{string(v1alpha1.Unhealthy)},
		},
	}

	for k, v := range fixtures {
		t.Run(k, func(t *testing.T) {
			m := agtesting.NewMocks()
			done := make(chan bool)

			m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
				gs := v1alpha1.GameServer{
					ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
					Status: v1alpha1.GameServerStatus{
						State: v1alpha1.Starting,
					},
				}
				return true, &v1alpha1.GameServerList{Items: []v1alpha1.GameServer{gs}}, nil
			})
			m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
				defer close(done)
				ua := action.(k8stesting.UpdateAction)
				gs := ua.GetObject().(*v1alpha1.GameServer)

				assert.Equal(t, v.state, gs.Status.State)

				return true, gs, nil
			})

			sc, err := NewSDKServer("test", "default",
				false, time.Second, 1, 0, m.KubeClient, m.AgonesClient)
			assert.Nil(t, err)
			sc.recorder = m.FakeRecorder

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			go sc.Run(ctx.Done())
			v.f(sc, ctx)
			timeout := time.After(10 * time.Second)

			select {
			case <-done:
			case <-timeout:
				assert.Fail(t, "Timeout on Run")
			}

			for _, str := range v.recordings {
				agtesting.AssertEventContains(t, m.FakeRecorder.Events, str)
			}
		})
	}
}

func TestSidecarUpdateState(t *testing.T) {
	t.Parallel()

	t.Run("ignore state change when unhealthy", func(t *testing.T) {
		m := agtesting.NewMocks()
		sc, err := defaultSidecar(m)
		assert.Nil(t, err)

		updated := false

		m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gs := v1alpha1.GameServer{
				ObjectMeta: metav1.ObjectMeta{Name: sc.gameServerName, Namespace: sc.namespace},
				Status: v1alpha1.GameServerStatus{
					State: v1alpha1.Unhealthy,
				},
			}
			return true, &v1alpha1.GameServerList{Items: []v1alpha1.GameServer{gs}}, nil
		})
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			updated = true
			return true, nil, nil
		})

		stop := make(chan struct{})
		defer close(stop)
		sc.informerFactory.Start(stop)
		assert.True(t, cache.WaitForCacheSync(stop, sc.gameServerSynced))

		err = sc.updateState(v1alpha1.Ready)
		assert.Nil(t, err)
		assert.False(t, updated)
	})
}

func TestSidecarHealthLastUpdated(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	m := agtesting.NewMocks()

	sc, err := defaultSidecar(m)
	assert.Nil(t, err)
	sc.healthDisabled = false
	fc := clock.NewFakeClock(now)
	sc.clock = fc

	stream := newEmptyMockStream()

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		err := sc.Health(stream) // nolint: vetshadow
		assert.Nil(t, err)
		wg.Done()
	}()

	// Test once with a single message
	fc.Step(3 * time.Second)
	stream.msgs <- &sdk.Empty{}

	err = waitForMessage(sc)
	assert.Nil(t, err)
	sc.healthMutex.RLock()
	assert.Equal(t, sc.clock.Now().UTC().String(), sc.healthLastUpdated.String())
	sc.healthMutex.RUnlock()

	// Test again, since the value has been set, that it is re-set
	fc.Step(3 * time.Second)
	stream.msgs <- &sdk.Empty{}
	err = waitForMessage(sc)
	assert.Nil(t, err)
	sc.healthMutex.RLock()
	assert.Equal(t, sc.clock.Now().UTC().String(), sc.healthLastUpdated.String())
	sc.healthMutex.RUnlock()

	// make sure closing doesn't change the time
	fc.Step(3 * time.Second)
	close(stream.msgs)
	assert.NotEqual(t, sc.clock.Now().UTC().String(), sc.healthLastUpdated.String())

	wg.Wait()
}

func TestSidecarHealthy(t *testing.T) {
	t.Parallel()

	m := agtesting.NewMocks()
	sc, err := defaultSidecar(m)
	assert.Nil(t, err)

	now := time.Now().UTC()
	fc := clock.NewFakeClock(now)
	sc.clock = fc

	stream := newEmptyMockStream()

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		err := sc.Health(stream) // nolint: vetshadow
		assert.Nil(t, err)
		wg.Done()
	}()

	fixtures := map[string]struct {
		disabled        bool
		timeAdd         time.Duration
		expectedHealthy bool
	}{
		"disabled, under timeout": {disabled: true, timeAdd: time.Second, expectedHealthy: true},
		"disabled, over timeout":  {disabled: true, timeAdd: 15 * time.Second, expectedHealthy: true},
		"enabled, under timeout":  {disabled: false, timeAdd: time.Second, expectedHealthy: true},
		"enabled, over timeout":   {disabled: false, timeAdd: 15 * time.Second, expectedHealthy: false},
	}

	for k, v := range fixtures {
		t.Run(k, func(t *testing.T) {
			logrus.WithField("test", k).Infof("Test Running")
			sc.healthDisabled = v.disabled
			fc.SetTime(time.Now().UTC())
			stream.msgs <- &sdk.Empty{}
			err = waitForMessage(sc)
			assert.Nil(t, err)

			fc.Step(v.timeAdd)
			sc.checkHealth()
			assert.Equal(t, v.expectedHealthy, sc.healthy())
		})
	}

	t.Run("initial delay", func(t *testing.T) {
		sc.healthDisabled = false
		fc.SetTime(time.Now().UTC())
		sc.initHealthLastUpdated(0)
		sc.healthFailureCount = 0
		sc.checkHealth()
		assert.True(t, sc.healthy())

		sc.initHealthLastUpdated(10 * time.Second)
		sc.checkHealth()
		assert.True(t, sc.healthy())
		fc.Step(9 * time.Second)
		sc.checkHealth()
		assert.True(t, sc.healthy())

		fc.Step(10 * time.Second)
		sc.checkHealth()
		assert.False(t, sc.healthy())
	})

	t.Run("health failure threshold", func(t *testing.T) {
		sc.healthDisabled = false
		sc.healthFailureThreshold = 3
		fc.SetTime(time.Now().UTC())
		sc.initHealthLastUpdated(0)
		sc.healthFailureCount = 0

		sc.checkHealth()
		assert.True(t, sc.healthy())
		assert.Equal(t, int64(0), sc.healthFailureCount)

		fc.Step(10 * time.Second)
		sc.checkHealth()
		assert.True(t, sc.healthy())
		sc.checkHealth()
		assert.True(t, sc.healthy())
		sc.checkHealth()
		assert.False(t, sc.healthy())

		stream.msgs <- &sdk.Empty{}
		err = waitForMessage(sc)
		assert.Nil(t, err)
		fc.Step(10 * time.Second)
		assert.True(t, sc.healthy())
	})

	close(stream.msgs)
	wg.Wait()
}

func TestSidecarHTTPHealthCheck(t *testing.T) {
	m := agtesting.NewMocks()
	sc, err := NewSDKServer("test", "default",
		false, 1*time.Second, 1, 0, m.KubeClient, m.AgonesClient)
	assert.Nil(t, err)
	now := time.Now().Add(time.Hour).UTC()
	fc := clock.NewFakeClock(now)
	// now we control time - so slow machines won't fail anymore
	sc.clock = fc
	sc.healthLastUpdated = now
	sc.healthFailureCount = 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sc.Run(ctx.Done())

	testHTTPHealth(t, "http://localhost:8080/healthz", "ok", http.StatusOK)
	testHTTPHealth(t, "http://localhost:8080/gshealthz", "ok", http.StatusOK)
	step := 2 * time.Second
	fc.Step(step)
	time.Sleep(step)
	testHTTPHealth(t, "http://localhost:8080/gshealthz", "", http.StatusInternalServerError)
}

func TestSDKServerConvert(t *testing.T) {
	t.Parallel()

	fixture := &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.Now(),
			Namespace:         "default",
			Name:              "test",
			Labels:            map[string]string{"foo": "bar"},
			Annotations:       map[string]string{"stuff": "things"},
			UID:               "1234",
		},
		Spec: v1alpha1.GameServerSpec{
			Health: v1alpha1.Health{
				Disabled:            false,
				InitialDelaySeconds: 10,
				FailureThreshold:    15,
				PeriodSeconds:       20,
			},
		},
		Status: v1alpha1.GameServerStatus{
			NodeName: "george",
			Address:  "127.0.0.1",
			State:    "Ready",
			Ports: []v1alpha1.GameServerStatusPort{
				{Name: "default", Port: 12345},
				{Name: "beacon", Port: 123123},
			},
		},
	}

	m := agtesting.NewMocks()
	sc, err := defaultSidecar(m)
	assert.Nil(t, err)

	eq := func(t *testing.T, fixture *v1alpha1.GameServer, sdkGs *sdk.GameServer) {
		assert.Equal(t, fixture.ObjectMeta.Name, sdkGs.ObjectMeta.Name)
		assert.Equal(t, fixture.ObjectMeta.Namespace, sdkGs.ObjectMeta.Namespace)
		assert.Equal(t, fixture.ObjectMeta.CreationTimestamp.Unix(), sdkGs.ObjectMeta.CreationTimestamp)
		assert.Equal(t, string(fixture.ObjectMeta.UID), sdkGs.ObjectMeta.Uid)
		assert.Equal(t, fixture.ObjectMeta.Labels, sdkGs.ObjectMeta.Labels)
		assert.Equal(t, fixture.ObjectMeta.Annotations, sdkGs.ObjectMeta.Annotations)
		assert.Equal(t, fixture.Spec.Health.Disabled, sdkGs.Spec.Health.Disabled)
		assert.Equal(t, fixture.Spec.Health.InitialDelaySeconds, sdkGs.Spec.Health.InitialDelaySeconds)
		assert.Equal(t, fixture.Spec.Health.FailureThreshold, sdkGs.Spec.Health.FailureThreshold)
		assert.Equal(t, fixture.Spec.Health.PeriodSeconds, sdkGs.Spec.Health.PeriodSeconds)
		assert.Equal(t, fixture.Status.Address, sdkGs.Status.Address)
		assert.Equal(t, string(fixture.Status.State), sdkGs.Status.State)
		assert.Len(t, sdkGs.Status.Ports, len(fixture.Status.Ports))
		for i, fp := range fixture.Status.Ports {
			p := sdkGs.Status.Ports[i]
			assert.Equal(t, fp.Name, p.Name)
			assert.Equal(t, fp.Port, p.Port)
		}
	}

	sdkGs := sc.convert(fixture)
	eq(t, fixture, sdkGs)
	assert.Zero(t, sdkGs.ObjectMeta.DeletionTimestamp)

	now := metav1.Now()
	fixture.DeletionTimestamp = &now
	sdkGs = sc.convert(fixture)
	eq(t, fixture, sdkGs)
	assert.Equal(t, fixture.ObjectMeta.DeletionTimestamp.Unix(), sdkGs.ObjectMeta.DeletionTimestamp)
}

func TestSDKServerGetGameServer(t *testing.T) {
	t.Parallel()

	fixture := &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Status: v1alpha1.GameServerStatus{
			State: v1alpha1.Ready,
		},
	}

	m := agtesting.NewMocks()
	m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &v1alpha1.GameServerList{Items: []v1alpha1.GameServer{*fixture}}, nil
	})

	stop := make(chan struct{})
	defer close(stop)

	sc, err := defaultSidecar(m)
	assert.Nil(t, err)

	sc.informerFactory.Start(stop)
	assert.True(t, cache.WaitForCacheSync(stop, sc.gameServerSynced))

	result, err := sc.GetGameServer(context.Background(), &sdk.Empty{})
	assert.Nil(t, err)
	assert.Equal(t, fixture.ObjectMeta.Name, result.ObjectMeta.Name)
	assert.Equal(t, fixture.ObjectMeta.Namespace, result.ObjectMeta.Namespace)
	assert.Equal(t, string(fixture.Status.State), result.Status.State)
}

func TestSDKServerWatchGameServer(t *testing.T) {
	t.Parallel()
	m := agtesting.NewMocks()
	sc, err := defaultSidecar(m)
	assert.Nil(t, err)
	assert.Empty(t, sc.connectedStreams)

	stream := newGameServerMockStream()
	asyncWatchGameServer(t, sc, stream)
	assert.Nil(t, waitConnectedStreamCount(sc, 1))
	assert.Equal(t, stream, sc.connectedStreams[0])

	stream = newGameServerMockStream()
	asyncWatchGameServer(t, sc, stream)
	assert.Nil(t, waitConnectedStreamCount(sc, 2))
	assert.Len(t, sc.connectedStreams, 2)
	assert.Equal(t, stream, sc.connectedStreams[1])
}

func TestSDKServerSendGameServerUpdate(t *testing.T) {
	t.Parallel()
	m := agtesting.NewMocks()
	sc, err := defaultSidecar(m)
	assert.Nil(t, err)
	assert.Empty(t, sc.connectedStreams)

	stop := make(chan struct{})
	defer close(stop)
	sc.stop = stop

	stream := newGameServerMockStream()
	asyncWatchGameServer(t, sc, stream)
	assert.Nil(t, waitConnectedStreamCount(sc, 1))

	fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test-server"}}

	sc.sendGameServerUpdate(fixture)

	var sdkGS *sdk.GameServer
	select {
	case sdkGS = <-stream.msgs:
	case <-time.After(3 * time.Second):
		assert.Fail(t, "Event stream should not have timed out")
	}

	assert.Equal(t, fixture.ObjectMeta.Name, sdkGS.ObjectMeta.Name)
}

func waitConnectedStreamCount(sc *SDKServer, count int) error {
	return wait.PollImmediate(1*time.Second, 10*time.Second, func() (bool, error) {
		sc.streamMutex.RLock()
		defer sc.streamMutex.RUnlock()
		return len(sc.connectedStreams) == count, nil
	})
}

func asyncWatchGameServer(t *testing.T, sc *SDKServer, stream *gameServerMockStream) {
	go func() {
		err := sc.WatchGameServer(&sdk.Empty{}, stream)
		assert.Nil(t, err)
	}()
}

func TestSDKServerUpdateEventHandler(t *testing.T) {
	t.Parallel()
	m := agtesting.NewMocks()

	fakeWatch := watch.NewFake()
	m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(fakeWatch, nil))

	stop := make(chan struct{})
	defer close(stop)

	sc, err := defaultSidecar(m)
	assert.Nil(t, err)

	sc.informerFactory.Start(stop)
	assert.True(t, cache.WaitForCacheSync(stop, sc.gameServerSynced))

	stream := newGameServerMockStream()
	asyncWatchGameServer(t, sc, stream)
	assert.Nil(t, waitConnectedStreamCount(sc, 1))

	fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test-server", Namespace: "default"},
		Spec: v1alpha1.GameServerSpec{},
	}

	// need to add it before it can be modified
	fakeWatch.Add(fixture.DeepCopy())
	fakeWatch.Modify(fixture.DeepCopy())

	var sdkGS *sdk.GameServer
	select {
	case sdkGS = <-stream.msgs:
	case <-time.After(3 * time.Second):
		assert.Fail(t, "Event stream should not have timed out")
	}

	assert.NotNil(t, sdkGS)
	assert.Equal(t, fixture.ObjectMeta.Name, sdkGS.ObjectMeta.Name)
}

func defaultSidecar(mocks agtesting.Mocks) (*SDKServer, error) {
	return NewSDKServer("test", "default",
		true, 5*time.Second, 1, 0, mocks.KubeClient, mocks.AgonesClient)
}

func waitForMessage(sc *SDKServer) error {
	return wait.PollImmediate(time.Second, 5*time.Second, func() (done bool, err error) {
		sc.healthMutex.RLock()
		defer sc.healthMutex.RUnlock()
		return sc.clock.Now().UTC() == sc.healthLastUpdated, nil
	})
}
