package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"net"

	"sync"

	ossignal "os/signal"
	"syscall"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	xds "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

// UpstreamPorts is a type that implements flag.Value interface
type UpstreamPorts []int

// String is a method that implements the flag.Value interface
func (u *UpstreamPorts) String() string {
	// See: https://stackoverflow.com/a/37533144/609290
	return strings.Join(strings.Fields(fmt.Sprint(*u)), ",")
}

// Set is a method that implements the flag.Value interface
func (u *UpstreamPorts) Set(port string) error {
	log.Printf("[UpstreamPorts] %s", port)
	i, err := strconv.Atoi(port)
	if err != nil {
		return err
	}
	*u = append(*u, i)
	return nil
}

var (
	debug       bool
	onlyLogging bool

	port        uint
	gatewayPort uint
	alsPort     uint

	mode string

	version int32

	cache cachev3.SnapshotCache

	upstreamPorts UpstreamPorts
)

const (
	localhost       = "127.0.0.1"
	Ads             = "ads"
	backendHostName = "127.0.0.1"
	listenerName    = "be-srv"
	routeConfigName = "be-srv-route"
	clusterName     = "be-srv-cluster"
	virtualHostName = "be-srv-vs"
)

func init() {
	flag.BoolVar(&debug, "debug", true, "Use debug logging")
	flag.UintVar(&port, "port", 18000, "Management server port")
	flag.UintVar(&gatewayPort, "gateway", 18001, "Management server port for HTTP gateway")
	flag.StringVar(&mode, "ads", Ads, "Management server type (ads, xds, rest)")
	// Converts repeated flags (e.g. `--upstream_port=50051 --upstream_port=50052`) into a []int
	flag.Var(&upstreamPorts, "upstream_port", "list of upstream gRPC servers")
}

type logger struct{}

func (logger logger) Infof(format string, args ...interface{}) {
	log.Infof(format, args...)
}
func (logger logger) Errorf(format string, args ...interface{}) {
	log.Errorf(format, args...)
}
func (cb *callbacks) Report() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	log.WithFields(log.Fields{"fetches": cb.fetches, "requests": cb.requests}).Info("cb.Report()  callbacks")
}
func (cb *callbacks) OnStreamOpen(ctx context.Context, id int64, typ string) error {
	log.Infof("OnStreamOpen %d open for Type [%s]", id, typ)
	return nil
}
func (cb *callbacks) OnStreamClosed(id int64) {
	log.Infof("OnStreamClosed %d closed", id)
}
func (cb *callbacks) OnStreamRequest(id int64, r *discovery.DiscoveryRequest) error {
	log.Infof("OnStreamRequest %d  Request[%v]", id, r.TypeUrl)
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.requests++
	if cb.signal != nil {
		close(cb.signal)
		cb.signal = nil
	}
	return nil
}
func (cb *callbacks) OnStreamResponse(ctx context.Context, id int64, req *discovery.DiscoveryRequest, resp *discovery.DiscoveryResponse) {
	log.Infof("OnStreamResponse... %d   Request [%v],  Response[%v]", id, req.TypeUrl, resp.TypeUrl)
	cb.Report()
}
func (cb *callbacks) OnFetchRequest(ctx context.Context, req *discovery.DiscoveryRequest) error {
	log.Infof("OnFetchRequest... Request [%v]", req.TypeUrl)
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.fetches++
	if cb.signal != nil {
		close(cb.signal)
		cb.signal = nil
	}
	return nil
}
func (cb *callbacks) OnFetchResponse(req *discovery.DiscoveryRequest, resp *discovery.DiscoveryResponse) {
	log.Infof("OnFetchResponse... Resquest[%v],  Response[%v]", req.TypeUrl, resp.TypeUrl)
}

func (cb *callbacks) OnDeltaStreamClosed(id int64) {
	log.Infof("OnDeltaStreamClosed... %v", id)
}

func (cb *callbacks) OnDeltaStreamOpen(ctx context.Context, id int64, typ string) error {
	log.Infof("OnDeltaStreamOpen... %v  of type %s", id, typ)
	return nil
}

func (c *callbacks) OnStreamDeltaRequest(i int64, request *discovery.DeltaDiscoveryRequest) error {
	log.Infof("OnStreamDeltaRequest... %v  of type %s", i, request)
	return nil
}

func (c *callbacks) OnStreamDeltaResponse(i int64, request *discovery.DeltaDiscoveryRequest, response *discovery.DeltaDiscoveryResponse) {
	log.Infof("OnStreamDeltaResponse... %v  of type %s", i, request)
}

type callbacks struct {
	signal   chan struct{}
	fetches  int
	requests int
	mu       sync.Mutex
}

// Hasher returns node ID as an ID
type Hasher struct {
}

// ID function
func (h Hasher) ID(node *core.Node) string {
	if node == nil {
		return "unknown"
	}
	return node.Id
}

const grpcMaxConcurrentStreams = 1000

// RunManagementServer starts an xDS server at the given port.
func RunManagementServer(ctx context.Context, server xds.Server, port uint) {
	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions, grpc.MaxConcurrentStreams(grpcMaxConcurrentStreams))
	grpcServer := grpc.NewServer(grpcOptions...)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.WithError(err).Fatal("failed to listen")
	}

	// register services
	discovery.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)

	log.WithFields(log.Fields{"port": port}).Info("management server listening")
	go func() {
		if err = grpcServer.Serve(lis); err != nil {
			log.Error(err)
		}
	}()
	<-ctx.Done()

	grpcServer.GracefulStop()
}

// 11/11/20 TODO:  optionally set this up
// // RunManagementGateway starts an HTTP gateway to an xDS server.
// func RunManagementGateway(ctx context.Context, srv xds.Server, port uint) {
// 	log.WithFields(log.Fields{"port": port}).Info("gateway listening HTTP/1.1")

// 	server := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: &xds.HTTPGateway{Server: srv}}
// 	go func() {
// 		if err := server.ListenAndServe(); err != nil {
// 			log.Error(err)
// 		}
// 	}()
// }

func main() {
	flag.Parse()
	if debug {
		log.SetLevel(log.DebugLevel)
	}
	ctx := context.Background()

	log.Printf("Starting control plane")

	signal := make(chan struct{})
	cb := &callbacks{
		signal:   signal,
		fetches:  0,
		requests: 0,
	}
	cache = cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil)

	srv := xds.NewServer(ctx, cache, cb)

	go RunManagementServer(ctx, srv, port)
	//go RunManagementGateway(ctx, srv, gatewayPort)

	<-signal

	cb.Report()

	nodeId := cache.GetStatusKeys()[0]
	log.Infof(">>>>>>>>>>>>>>>>>>> creating NodeID %s", nodeId)

	eds := &endpoint.ClusterLoadAssignment{}
	cds := &cluster.Cluster{}
	rds := &route.RouteConfiguration{}
	lds := &listener.Listener{}

	ldsF, err := os.ReadFile("lds")
	if err != nil {
		log.Fatalf("error:", err)
	}
	rdsF, err := os.ReadFile("rds")
	if err != nil {
		log.Fatalf("error:", err)
	}
	cdsF, err := os.ReadFile("cds")
	if err != nil {
		log.Fatalf("error:", err)
	}
	edsF, err := os.ReadFile("eds")
	if err != nil {
		log.Fatalf("error:", err)
	}

	err = protojson.Unmarshal(edsF, eds)
	if err != nil {
		log.Fatalf("error:", err)
	}
	err = protojson.Unmarshal(cdsF, cds)
	if err != nil {
		log.Fatalf("error:", err)
	}
	err = protojson.Unmarshal(rdsF, rds)
	if err != nil {
		log.Fatalf("error:", err)
	}
	err = protojson.Unmarshal(ldsF, lds)
	if err != nil {
		log.Fatalf("error:", err)
	}

	resources := make(map[resource.Type][]types.Resource, 4)
	resources[resource.ClusterType] = []types.Resource{cds}
	resources[resource.ListenerType] = []types.Resource{lds}
	resources[resource.RouteType] = []types.Resource{rds}
	resources[resource.EndpointType] = []types.Resource{eds}

	snap, err := cachev3.NewSnapshot(fmt.Sprint(version), resources)
	if err != nil {
		log.Fatalf("Could not set snapshot %v", err)
	}
	err = cache.SetSnapshot(ctx, nodeId, snap)
	if err != nil {
		log.Fatalf("Could not set snapshot %v", err)
	}

	sigs := make(chan os.Signal, 1)
	defer close(sigs)
	ossignal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan bool, 1)
	go func() {
		sig := <-sigs
		fmt.Println()
		fmt.Println(sig)
		done <- true
	}()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal("NewWatcher failed: ", err)
	}
	defer watcher.Close()
	go func() {
		defer close(done)

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				log.Printf("%s %s\n", event.Name, event.Op)
				edsF, err = os.ReadFile("eds")
				if err != nil {
					log.Fatalf("error:", err)
				}
				fmt.Println(string(edsF))
				err = protojson.Unmarshal(edsF, eds)
				if err != nil {
					log.Fatalf("error:", err)
				}
				resources[resource.EndpointType] = []types.Resource{eds}
				atomic.AddInt32(&version, 1)
				snap, err := cachev3.NewSnapshot(fmt.Sprint(version), resources)
				if err != nil {
					log.Fatalf("Could not set snapshot %v", err)
				}
				err = cache.SetSnapshot(ctx, nodeId, snap)
				if err != nil {
					log.Fatalf("Could not set snapshot %v", err)
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			}
		}

	}()
	err = watcher.Add("eds")
	if err != nil {
		log.Fatal("Add failed:", err)
	}

	fmt.Println("awaiting signal")
	<-done
	fmt.Println("exiting")
}
