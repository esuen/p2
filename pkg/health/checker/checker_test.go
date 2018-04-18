package checker

import (
	"context"
	"testing"
	"time"

	. "github.com/anthonybishopric/gotcha"
	"github.com/square/p2/pkg/health"
	hc "github.com/square/p2/pkg/health/client"
	"github.com/square/p2/pkg/manifest"
	"github.com/square/p2/pkg/store/consul"
	"github.com/square/p2/pkg/types"
)

type fakeConsulStore struct {
	results map[string]consul.WatchResult
}

func (f fakeConsulStore) GetHealth(service string, node types.NodeName) (consul.WatchResult, error) {
	return f.results[node.String()], nil
}
func (f fakeConsulStore) GetServiceHealth(service string) (map[string]consul.WatchResult, error) {
	return f.results, nil
}

type fakeHealthClient struct {
	HealthResponses map[string]hc.HealthResponse
}

func (f fakeHealthClient) HealthCheck(ctx context.Context, req *hc.HealthRequest) (health.HealthState, error) {
	return f.HealthResponses[req.Url].Health, nil
}

func (f fakeHealthClient) HealthMonitor(ctx context.Context, req *hc.HealthRequest, resultCh chan *hc.HealthResponse) error {
	timer := time.NewTimer(time.Second * 1)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if healthResponse, ok := f.HealthResponses[req.Url]; ok {
				resultCh <- &healthResponse
				continue
			}
			resultCh <- &hc.HealthResponse{
				HealthRequest: *req,
				Health:        health.Unknown,
				Error:         nil,
			}
		}
	}
}

func (f fakeHealthClient) HealthCheckEndpoints(ctx context.Context, req *hc.HealthEndpointsRequest) (map[string]health.HealthState, error) {
	ret := make(map[string]health.HealthState)
	for _, endpoint := range req.Endpoints {
		healthResponse, ok := f.HealthResponses[endpoint]
		if ok {
			ret[endpoint] = healthResponse.Health
		} else {
			ret[endpoint] = health.Unknown
		}
	}
	return ret, nil
}

func TestNodeIDsToStatusEndpoints(t *testing.T) {
	nodeIDs := []types.NodeName{"node1"}
	statusStanza := manifest.StatusStanza{Port: 1}
	expected := []string{
		"https://node1:1/_status",
	}
	statusEndpoints := nodeIDsToStatusEndpoints(nodeIDs, statusStanza)
	if len(statusEndpoints) != len(nodeIDs) {
		t.Fatalf("Expected length of output of nodeIDsToStatusEndpoints to equal length of input nodeIDs. Expected %d but got %d", len(nodeIDs), len(statusEndpoints))
	}
	if statusEndpoints[0] != expected[0] {
		t.Fatalf("Expected statusEndpoint to be %s but got %s", expected[0], statusEndpoints[0])
	}

	statusStanza = manifest.StatusStanza{
		HTTP: true,
		Path: "path",
		Port: 1,
	}
	expected = []string{
		"http://node1:1/path",
	}
	statusEndpoints = nodeIDsToStatusEndpoints(nodeIDs, statusStanza)
	if statusEndpoints[0] != expected[0] {
		t.Fatalf("Expected statusEndpoint to be %s but got %s", expected[0], statusEndpoints[0])
	}
}

func TestStatusURLToNodeName(t *testing.T) {
	nodeIDs := []types.NodeName{"node1"}
	statusStanza := manifest.StatusStanza{
		Port: 1,
	}
	statusEndpoints := nodeIDsToStatusEndpoints(nodeIDs, statusStanza)
	nodeID, err := statusURLToNodeName(statusEndpoints[0])
	if err != nil {
		t.Fatalf("Unexpected error in statusURLToNodeName: %v", err)
	}
	if nodeID != nodeIDs[0] {
		t.Fatalf("Expected nodeID to be %s but got %s", nodeIDs[0], nodeID)
	}
}

func TestWatchPodOnNode(t *testing.T) {
	nodeID := types.NodeName("node1")
	podID := types.PodID("pod1")
	statusStanza := manifest.StatusStanza{Port: 1}
	expected := health.Critical
	fakeHealthClient := fakeHealthClient{
		HealthResponses: make(map[string]hc.HealthResponse),
	}
	endpoint := nodeIDToStatusEndpoint(nodeID, statusStanza)
	fakeHealthClient.HealthResponses[endpoint] = hc.HealthResponse{
		HealthRequest: hc.HealthRequest{
			Url:      endpoint,
			Protocol: "https",
		},
		Health: expected,
		Error:  nil,
	}
	hChecker := NewHealthChecker(fakeHealthClient, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	resultCh, _ := hChecker.WatchPodOnNode(ctx, nodeID, podID, statusStanza)

LOOP1:
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("WatchPodOnNode test took longer than expected to receive result")
		case result := <-resultCh:
			if result.Status != expected {
				t.Fatalf("Expected health result %s in WatchPodOnNode but got %s instead", expected, result.Status)
			}
			break LOOP1
		}
	}

	// test always healthy
	statusStanza.Port = 0
	expected = health.Passing
	ctx, cancel = context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	resultCh, _ = hChecker.WatchPodOnNode(ctx, nodeID, podID, statusStanza)

LOOP2:
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("WatchPodOnNode test took longer than expected to receive result")
		case result := <-resultCh:
			if result.Status != expected {
				t.Fatalf("Expected health result %s in WatchPodOnNode but got %s instead", expected, result.Status)
			}
			break LOOP2
		}
	}
}

// using fake health service
func TestWatchService(t *testing.T) {
	// typical test where all nodes are all critical
	nodeIDs := []types.NodeName{"node1", "node2", "node3"}
	statusStanza := manifest.StatusStanza{Port: 1}
	expected := make(map[types.NodeName]health.HealthState)
	for _, nodeID := range nodeIDs {
		expected[nodeID] = health.Critical
	}

	fakeHealthClient := fakeHealthClient{
		HealthResponses: make(map[string]hc.HealthResponse),
	}

	endpoints := nodeIDsToStatusEndpoints(nodeIDs, statusStanza)
	for _, endpoint := range endpoints {
		nodeID, err := statusURLToNodeName(endpoint)
		if err != nil {
			t.Fatalf("Error calling statusURLToNodeName: %v", err)
		}
		fakeHealthClient.HealthResponses[endpoint] = hc.HealthResponse{
			HealthRequest: hc.HealthRequest{
				Url:      endpoint,
				Protocol: "https",
			},
			Health: expected[nodeID],
			Error:  nil,
		}
	}

	serviceNodes := func(serviceID string) ([]types.NodeName, error) {
		return nodeIDs, nil
	}
	hChecker := NewHealthChecker(fakeHealthClient, serviceNodes)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	resultCh := make(chan map[types.NodeName]health.Result)
	errCh := make(chan error)
	watchDelay := 1 * time.Second

	// start WatchService goroutine
	go hChecker.WatchService(ctx, "serviceID", resultCh, errCh, watchDelay, statusStanza)

	result := make(map[types.NodeName]health.Result)

LOOP1:
	for {
		select {
		case <-ctx.Done():
			return
		case result = <-resultCh:
			// get expected HealthResponses set in fakeHealthClient
			for _, nodeID := range nodeIDs {
				healthResult, ok := result[nodeID]
				if !ok {
					t.Fatalf("Expected nodeID %s to be in results but not found", nodeID)
				}
				if expected[nodeID] != healthResult.Status {
					t.Fatalf("Expected hCheck status from WatchService to be %s but got %s instead", expected[nodeID], healthResult.Status)
				}
			}
			break LOOP1
		}
	}

	// test do not receive health result when
}

// using health service
func TestService(t *testing.T) {
	nodeIDs := []types.NodeName{"node1"}
	statusStanza := manifest.StatusStanza{
		Port: 1,
	}
	expected := make(map[types.NodeName]health.HealthState)
	for _, nodeID := range nodeIDs {
		expected[nodeID] = health.Passing
	}

	fakeHealthClient := fakeHealthClient{
		HealthResponses: make(map[string]hc.HealthResponse),
	}
	endpoints := nodeIDsToStatusEndpoints(nodeIDs, statusStanza)
	for _, endpoint := range endpoints {
		nodeID, err := statusURLToNodeName(endpoint)
		if err != nil {
			t.Fatalf("Error calling statusURLToNodeName: %v", err)
		}
		fakeHealthClient.HealthResponses[endpoint] = hc.HealthResponse{
			HealthRequest: hc.HealthRequest{
				Url:      endpoint,
				Protocol: "https",
			},
			Health: expected[nodeID],
			Error:  nil,
		}
	}

	serviceNodes := func(serviceID string) ([]types.NodeName, error) {
		return nodeIDs, nil
	}
	hChecker := NewHealthChecker(fakeHealthClient, serviceNodes)
	healthResults, err := hChecker.Service("serviceID", statusStanza)
	if err != nil {
		t.Fatalf("Unexpected error calling healthChecker Service: %v", err)
	}
	if len(healthResults) != len(nodeIDs) {
		t.Fatalf("Expected length of healthResults from healthChecker Service to be %d but got %d", len(nodeIDs), len(healthResults))
	}

	// get expected HealthResponses set in fakeHealthClient
	for _, nodeID := range nodeIDs {
		healthResult, ok := healthResults[nodeID]
		if !ok {
			t.Fatalf("Expected nodeID %s to be in results but not found", nodeID)
		}
		if expected[nodeID] != healthResult.Status {
			t.Fatalf("Expected hCheck status from WatchService to be %s but got %s instead", expected[nodeID], healthResult.Status)
		}
	}
}

func TestConsulService(t *testing.T) {
	result1 := consul.WatchResult{
		Id:      "abc123",
		Node:    "node1",
		Service: "slug",
		Status:  "passing",
	}
	fakeStore := fakeConsulStore{
		results: map[string]consul.WatchResult{"node1": result1},
	}
	hc := consulHealthChecker{
		consulStore: fakeStore,
	}

	results, err := hc.Service("some_service", manifest.StatusStanza{})
	Assert(t).IsNil(err, "Unexpected error calling Service()")

	expected := health.Result{
		ID:      "abc123",
		Node:    "node1",
		Service: "slug",
		Status:  "passing",
	}
	Assert(t).AreEqual(results["node1"], expected, "Unexpected results calling Service()")
}
