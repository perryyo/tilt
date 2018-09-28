package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/windmilleng/tilt/internal/logger"
	"github.com/windmilleng/tilt/internal/synclet/sidecar"

	"github.com/pkg/errors"
	"github.com/windmilleng/tilt/internal/k8s"
	"github.com/windmilleng/tilt/internal/synclet"
	"google.golang.org/grpc"
)

const newClientTimeout = time.Second * 10

type newCliFn func(ctx context.Context, kCli k8s.Client, podID k8s.PodID) (synclet.SyncletClient, error)
type SidecarSyncletManager struct {
	kCli      k8s.Client
	mutex     *sync.Mutex
	clients   map[k8s.PodID]synclet.SyncletClient
	newClient newCliFn
}

type tunneledSyncletClient struct {
	synclet.SyncletClient
	tunnelCloser func()
}

var _ synclet.SyncletClient = tunneledSyncletClient{}

func (t tunneledSyncletClient) Close() error {
	err := t.SyncletClient.Close()
	if err != nil {
		return err
	}

	t.tunnelCloser()

	return nil
}

func NewSidecarSyncletManager(kCli k8s.Client) SidecarSyncletManager {
	return SidecarSyncletManager{
		kCli:      kCli,
		mutex:     new(sync.Mutex),
		clients:   make(map[k8s.PodID]synclet.SyncletClient),
		newClient: newSidecarSyncletClient,
	}
}

func NewSidecarSyncletManagerForTests(kCli k8s.Client, fakeCli synclet.SyncletClient) SidecarSyncletManager {
	newClientFn := func(ctx context.Context, kCli k8s.Client, podID k8s.PodID) (synclet.SyncletClient, error) {
		return fakeCli, nil
	}

	return SidecarSyncletManager{
		kCli:      kCli,
		mutex:     new(sync.Mutex),
		clients:   make(map[k8s.PodID]synclet.SyncletClient),
		newClient: newClientFn,
	}
}

func (ssm SidecarSyncletManager) ClientForPod(ctx context.Context, podID k8s.PodID) (synclet.SyncletClient, error) {
	ssm.mutex.Lock()
	defer ssm.mutex.Unlock()

	client, ok := ssm.clients[podID]
	if ok {
		return client, nil
	}

	client, err := ssm.pollForNewClient(ctx, ssm.kCli, podID, newClientTimeout)
	if err != nil {
		return nil, errors.Wrap(err, "error creating synclet client")
	}
	ssm.clients[podID] = client

	return client, nil
}

func (ssm SidecarSyncletManager) ForgetPod(ctx context.Context, podID k8s.PodID) error {
	ssm.mutex.Lock()
	defer ssm.mutex.Unlock()

	client, ok := ssm.clients[podID]
	if !ok {
		// if we don't know about the pod, it's already forgotten - noop
		return nil
	}

	delete(ssm.clients, podID)

	return client.Close()
}

func (ssm SidecarSyncletManager) pollForNewClient(ctx context.Context, kCli k8s.Client, podID k8s.PodID, timeout time.Duration) (cli synclet.SyncletClient, err error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "SidecarSyncletManager-pollForNewClient")
	defer span.Finish()

	start := time.Now()
	for time.Since(start) < timeout {
		// TODO(maia): better distinction between errs meaning "couldn't connect yet"
		// and "everything is borked, stop trying"
		cli, err = ssm.newClient(ctx, kCli, podID)
		if cli != nil {
			return cli, nil
		}
	}
	return nil, errors.Wrapf(err, "timed out trying to create new synclet client for pod %s (after %s) with err",
		podID.String(), timeout)
}
func newSidecarSyncletClient(ctx context.Context, kCli k8s.Client, podID k8s.PodID) (synclet.SyncletClient, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "SidecarSyncletManager-newSidecarSyncletClient")
	defer span.Finish()

	pod, err := kCli.PodByID(ctx, podID, k8s.DefaultNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "newSidecarSyncletClient")
	}

	// Make sure that the synclet container is ready and not crashlooping.
	_, err = k8s.WaitForContainerReady(ctx, kCli, pod, sidecar.SyncletImageRef)
	if err != nil {
		return nil, errors.Wrap(err, "newSidecarSyncletClient")
	}

	// TODO(nick): We need a better way to kill the client when the pod dies.
	tunneledPort, tunnelCloser, err := kCli.ForwardPort(ctx, k8s.DefaultNamespace, podID, synclet.Port)
	if err != nil {
		return nil, errors.Wrapf(err, "failed opening tunnel to synclet pod '%s'", podID)
	}

	logger.Get(ctx).Verbosef("i'm a sidecar - tunneling to synclet client at %s (local port %d)", podID.String(), tunneledPort)

	t := opentracing.GlobalTracer()

	conn, err := grpc.DialContext(ctx, fmt.Sprintf("127.0.0.1:%d", tunneledPort), grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(otgrpc.OpenTracingClientInterceptor(t)),
		grpc.WithStreamInterceptor(otgrpc.OpenTracingStreamClientInterceptor(t)))
	if err != nil {
		return nil, errors.Wrap(err, "connecting to synclet")
	}

	return tunneledSyncletClient{synclet.NewGRPCClient(conn), tunnelCloser}, nil
}