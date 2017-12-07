/*
Copyright 2015 The Kubernetes Authors.

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

package backends

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	computealpha "google.golang.org/api/compute/v0.alpha"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/ingress-gce/pkg/healthchecks"
	"k8s.io/ingress-gce/pkg/instances"
	"k8s.io/ingress-gce/pkg/networkendpointgroup"
	"k8s.io/ingress-gce/pkg/storage"
	"k8s.io/ingress-gce/pkg/utils"
)

const defaultZone = "zone-a"

var (
	defaultNamer  = utils.NewNamer("uid1", "fw1")
	existingProbe = &api_v1.Probe{
		Handler: api_v1.Handler{
			HTTPGet: &api_v1.HTTPGetAction{
				Scheme: api_v1.URISchemeHTTPS,
				Path:   "/my-special-path",
				Port: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: 443,
				},
			},
		},
	}
	noOpErrFunc = func(op int, be *compute.BackendService) error { return nil }
)

func newTestJig(f BackendServices, fakeIGs instances.InstanceGroups, syncWithCloud bool) (*Backends, healthchecks.HealthCheckProvider) {
	negGetter := networkendpointgroup.NewFakeNetworkEndpointGroupCloud("test-subnetwork", "test-network")
	nodePool := instances.NewNodePool(fakeIGs, defaultNamer)
	nodePool.Init(&instances.FakeZoneLister{Zones: []string{defaultZone}})
	healthCheckProvider := healthchecks.NewFakeHealthCheckProvider()
	healthChecks := healthchecks.NewHealthChecker(healthCheckProvider, "/", defaultNamer)
	bp := NewBackendPool(f, negGetter, healthChecks, nodePool, defaultNamer, []int64{}, syncWithCloud)
	probes := map[ServicePort]*api_v1.Probe{{Port: 443, Protocol: utils.ProtocolHTTPS}: existingProbe}
	bp.Init(NewFakeProbeProvider(probes))

	return bp, healthCheckProvider
}

func TestBackendPoolAdd(t *testing.T) {
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	pool, _ := newTestJig(f, fakeIGs, false)

	testCases := []ServicePort{
		{Port: 80, Protocol: utils.ProtocolHTTP},
		{Port: 443, Protocol: utils.ProtocolHTTPS},
	}

	for _, nodePort := range testCases {
		// For simplicity, these tests use 80/443 as nodeports
		t.Run(fmt.Sprintf("Port:%v Protocol:%v", nodePort.Port, nodePort.Protocol), func(t *testing.T) {
			// Add a backend for a port, then re-add the same port and
			// make sure it corrects a broken link from the backend to
			// the instance group.
			err := pool.Ensure([]ServicePort{nodePort}, nil)
			if err != nil {
				t.Fatalf("Did not find expect error when adding a nodeport: %v, err: %v", nodePort, err)
			}
			beName := defaultNamer.Backend(nodePort.Port)

			// Check that the new backend has the right port
			be, err := f.GetGlobalBackendService(beName)
			if err != nil {
				t.Fatalf("Did not find expected backend %v", beName)
			}
			if be.Port != nodePort.Port {
				t.Fatalf("Backend %v has wrong port %v, expected %v", be.Name, be.Port, nodePort)
			}

			// Check that the instance group has the new port.
			ig, err := fakeIGs.GetInstanceGroup(defaultNamer.InstanceGroup(), defaultZone)
			var found bool
			for _, port := range ig.NamedPorts {
				if port.Port == nodePort.Port {
					found = true
				}
			}
			if !found {
				t.Fatalf("Port %v not added to instance group", nodePort)
			}

			// Check the created healthcheck is the correct protocol
			hc, err := pool.healthChecker.Get(nodePort.Port, false)
			if err != nil {
				t.Fatalf("Unexpected err when querying fake healthchecker: %v", err)
			}

			if hc.Protocol() != nodePort.Protocol {
				t.Fatalf("Healthcheck scheme does not match nodeport scheme: hc:%v np:%v", hc.Protocol(), nodePort.Protocol)
			}

			if nodePort.Port == 443 && hc.RequestPath != "/my-special-path" {
				t.Fatalf("Healthcheck for 443 should have special request path from probe")
			}
		})
	}
}

func TestHealthCheckMigration(t *testing.T) {
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	pool, hcp := newTestJig(f, fakeIGs, false)

	p := ServicePort{Port: 7000, Protocol: utils.ProtocolHTTP}

	// Create a legacy health check and insert it into the HC provider.
	legacyHC := &compute.HttpHealthCheck{
		Name:               defaultNamer.Backend(p.Port),
		RequestPath:        "/my-healthz-path",
		Host:               "k8s.io",
		Description:        "My custom HC",
		UnhealthyThreshold: 30,
		CheckIntervalSec:   40,
	}
	hcp.CreateHttpHealthCheck(legacyHC)

	// Add the service port to the backend pool
	pool.Ensure([]ServicePort{p}, nil)

	// Assert the proper health check was created
	hc, _ := pool.healthChecker.Get(p.Port, false)
	if hc == nil || hc.Protocol() != p.Protocol {
		t.Fatalf("Expected %s health check, received %v: ", p.Protocol, hc)
	}

	// Assert the newer health check has the legacy health check settings
	if hc.RequestPath != legacyHC.RequestPath ||
		hc.Host != legacyHC.Host ||
		hc.UnhealthyThreshold != legacyHC.UnhealthyThreshold ||
		hc.CheckIntervalSec != legacyHC.CheckIntervalSec ||
		hc.Description != legacyHC.Description {
		t.Fatalf("Expected newer health check to have identical settings to legacy health check. Legacy: %+v, New: %+v", legacyHC, hc)
	}
}

func TestBackendPoolUpdate(t *testing.T) {
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	pool, _ := newTestJig(f, fakeIGs, false)

	p := ServicePort{Port: 3000, Protocol: utils.ProtocolHTTP}
	pool.Ensure([]ServicePort{p}, nil)
	beName := defaultNamer.Backend(p.Port)

	be, err := f.GetGlobalBackendService(beName)
	if err != nil {
		t.Fatalf("Unexpected err: %v", err)
	}

	if utils.AppProtocol(be.Protocol) != p.Protocol {
		t.Fatalf("Expected scheme %v but got %v", p.Protocol, be.Protocol)
	}

	// Assert the proper health check was created
	hc, _ := pool.healthChecker.Get(p.Port, false)
	if hc == nil || hc.Protocol() != p.Protocol {
		t.Fatalf("Expected %s health check, received %v: ", p.Protocol, hc)
	}

	// Update service port to encrypted
	p.Protocol = utils.ProtocolHTTPS
	pool.Ensure([]ServicePort{p}, nil)

	be, err = f.GetGlobalBackendService(beName)
	if err != nil {
		t.Fatalf("Unexpected err retrieving backend service after update: %v", err)
	}

	// Assert the backend has the correct protocol
	if utils.AppProtocol(be.Protocol) != p.Protocol {
		t.Fatalf("Expected scheme %v but got %v", p.Protocol, utils.AppProtocol(be.Protocol))
	}

	// Assert the proper health check was created
	hc, _ = pool.healthChecker.Get(p.Port, false)
	if hc == nil || hc.Protocol() != p.Protocol {
		t.Fatalf("Expected %s health check, received %v: ", p.Protocol, hc)
	}
}

func TestBackendPoolChaosMonkey(t *testing.T) {
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	pool, _ := newTestJig(f, fakeIGs, false)

	nodePort := ServicePort{Port: 8080, Protocol: utils.ProtocolHTTP}
	pool.Ensure([]ServicePort{nodePort}, nil)
	beName := defaultNamer.Backend(nodePort.Port)

	be, _ := f.GetGlobalBackendService(beName)

	// Mess up the link between backend service and instance group.
	// This simulates a user doing foolish things through the UI.
	be.Backends = []*compute.Backend{
		{Group: "test edge hop"},
	}
	f.calls = []int{}
	f.UpdateGlobalBackendService(be)

	pool.Ensure([]ServicePort{nodePort}, nil)
	for _, call := range f.calls {
		if call == utils.Create {
			t.Fatalf("Unexpected create for existing backend service")
		}
	}
	gotBackend, err := f.GetGlobalBackendService(beName)
	if err != nil {
		t.Fatalf("Failed to find a backend with name %v: %v", beName, err)
	}
	gotGroup, err := fakeIGs.GetInstanceGroup(defaultNamer.InstanceGroup(), defaultZone)
	if err != nil {
		t.Fatalf("Failed to find instance group %v", defaultNamer.InstanceGroup())
	}
	backendLinks := sets.NewString()
	for _, be := range gotBackend.Backends {
		backendLinks.Insert(be.Group)
	}
	if !backendLinks.Has(gotGroup.SelfLink) {
		t.Fatalf(
			"Broken instance group link, got: %+v expected: %v",
			backendLinks.List(),
			gotGroup.SelfLink)
	}
}

func TestBackendPoolSync(t *testing.T) {
	// Call sync on a backend pool with a list of ports, make sure the pool
	// creates/deletes required ports.
	svcNodePorts := []ServicePort{{Port: 81, Protocol: utils.ProtocolHTTP}, {Port: 82, Protocol: utils.ProtocolHTTPS}, {Port: 83, Protocol: utils.ProtocolHTTP}}
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	pool, _ := newTestJig(f, fakeIGs, true)
	pool.Ensure([]ServicePort{{Port: 81}}, nil)
	pool.Ensure([]ServicePort{{Port: 90}}, nil)
	if err := pool.Ensure(svcNodePorts, nil); err != nil {
		t.Errorf("Expected backend pool to add node ports, err: %v", err)
	}
	if err := pool.GC(svcNodePorts); err != nil {
		t.Errorf("Expected backend pool to GC, err: %v", err)
	}
	if _, err := pool.Get(90); err == nil {
		t.Fatalf("Did not expect to find port 90")
	}
	for _, port := range svcNodePorts {
		if _, err := pool.Get(port.Port); err != nil {
			t.Fatalf("Expected to find port %v", port)
		}
	}

	svcNodePorts = []ServicePort{{Port: 81}}
	deletedPorts := []ServicePort{{Port: 82}, {Port: 83}}
	if err := pool.GC(svcNodePorts); err != nil {
		t.Fatalf("Expected backend pool to GC, err: %v", err)
	}

	for _, port := range deletedPorts {
		if _, err := pool.Get(port.Port); err == nil {
			t.Fatalf("Pool contains %v after deletion", port)
		}
	}

	// All these backends should be ignored because they don't belong to the cluster.
	// foo - non k8s managed backend
	// k8s-be-foo - foo is not a nodeport
	// k8s--bar--foo - too many cluster delimiters
	// k8s-be-3001--uid - another cluster tagged with uid
	unrelatedBackends := sets.NewString([]string{"foo", "k8s-be-foo", "k8s--bar--foo", "k8s-be-30001--uid"}...)
	for _, name := range unrelatedBackends.List() {
		f.CreateGlobalBackendService(&compute.BackendService{Name: name})
	}

	// This backend should get deleted again since it is managed by this cluster.
	f.CreateGlobalBackendService(&compute.BackendService{Name: defaultNamer.Backend(deletedPorts[0].Port)})

	// TODO: Avoid casting.
	// Repopulate the pool with a cloud list, which now includes the 82 port
	// backend. This would happen if, say, an ingress backend is removed
	// while the controller is restarting.
	pool.snapshotter.(*storage.CloudListingPool).ReplenishPool()

	pool.GC(svcNodePorts)

	currBackends, _ := f.ListGlobalBackendServices()
	currSet := sets.NewString()
	for _, b := range currBackends.Items {
		currSet.Insert(b.Name)
	}
	// Port 81 still exists because it's an in-use service NodePort.
	knownBe := defaultNamer.Backend(81)
	if !currSet.Has(knownBe) {
		t.Fatalf("Expected %v to exist in backend pool", knownBe)
	}
	currSet.Delete(knownBe)
	if !currSet.Equal(unrelatedBackends) {
		t.Fatalf("Some unrelated backends were deleted. Expected %+v, got %+v", unrelatedBackends, currSet)
	}
}

func TestBackendPoolDeleteLegacyHealthChecks(t *testing.T) {
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	negGetter := networkendpointgroup.NewFakeNetworkEndpointGroupCloud("test-subnetwork", "test-network")
	nodePool := instances.NewNodePool(fakeIGs, defaultNamer)
	nodePool.Init(&instances.FakeZoneLister{Zones: []string{defaultZone}})
	hcp := healthchecks.NewFakeHealthCheckProvider()
	healthChecks := healthchecks.NewHealthChecker(hcp, "/", defaultNamer)
	bp := NewBackendPool(f, negGetter, healthChecks, nodePool, defaultNamer, []int64{}, false)
	probes := map[ServicePort]*api_v1.Probe{}
	bp.Init(NewFakeProbeProvider(probes))

	// Create a legacy HTTP health check
	beName := defaultNamer.Backend(80)
	if err := hcp.CreateHttpHealthCheck(&compute.HttpHealthCheck{
		Name: beName,
		Port: 80,
	}); err != nil {
		t.Fatalf("unexpected error creating http health check %v", err)
	}

	// Verify health check exists
	hc, err := hcp.GetHttpHealthCheck(beName)
	if err != nil {
		t.Fatalf("unexpected error getting http health check %v", err)
	}

	// Create backend service with expected name and link to legacy health check
	f.CreateGlobalBackendService(&compute.BackendService{
		Name:         beName,
		HealthChecks: []string{hc.SelfLink},
	})

	// Have pool sync the above backend service
	bp.Ensure([]ServicePort{{Port: 80, Protocol: utils.ProtocolHTTPS}}, nil)

	// Verify the legacy health check has been deleted
	_, err = hcp.GetHttpHealthCheck(beName)
	if err == nil {
		t.Fatalf("expected error getting http health check %v", err)
	}

	// Verify a newer health check exists
	hcNew, err := hcp.GetHealthCheck(beName)
	if err != nil {
		t.Fatalf("unexpected error getting http health check %v", err)
	}

	// Verify the newer health check is of type HTTPS
	if hcNew.Type != string(utils.ProtocolHTTPS) {
		t.Fatalf("expected health check type to be %v, actual %v", string(utils.ProtocolHTTPS), hcNew.Type)
	}
}

func TestBackendPoolShutdown(t *testing.T) {
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	pool, _ := newTestJig(f, fakeIGs, false)

	// Add a backend-service and verify that it doesn't exist after Shutdown()
	pool.Ensure([]ServicePort{{Port: 80}}, nil)
	pool.Shutdown()
	if _, err := f.GetGlobalBackendService(defaultNamer.Backend(80)); err == nil {
		t.Fatalf("%v", err)
	}
}

func TestBackendInstanceGroupClobbering(t *testing.T) {
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	pool, _ := newTestJig(f, fakeIGs, false)

	// This will add the instance group k8s-ig to the instance pool
	pool.Ensure([]ServicePort{{Port: 80}}, nil)

	be, err := f.GetGlobalBackendService(defaultNamer.Backend(80))
	if err != nil {
		t.Fatalf("f.GetGlobalBackendService(defaultNamer.Backend(80)) = _, %v, want _, nil", err)
	}
	// Simulate another controller updating the same backend service with
	// a different instance group
	newGroups := []*compute.Backend{
		{Group: "k8s-ig-bar"},
		{Group: "k8s-ig-foo"},
	}
	be.Backends = append(be.Backends, newGroups...)
	if err = f.UpdateGlobalBackendService(be); err != nil {
		t.Fatalf("Failed to update backend service %v", be.Name)
	}

	// Make sure repeated adds don't clobber the inserted instance group
	pool.Ensure([]ServicePort{{Port: 80}}, nil)
	be, err = f.GetGlobalBackendService(defaultNamer.Backend(80))
	if err != nil {
		t.Fatalf("%v", err)
	}
	gotGroups := sets.NewString()
	for _, g := range be.Backends {
		gotGroups.Insert(g.Group)
	}

	// seed expectedGroups with the first group native to this controller
	expectedGroups := sets.NewString("k8s-ig--uid1")
	for _, newGroup := range newGroups {
		expectedGroups.Insert(newGroup.Group)
	}
	if !expectedGroups.Equal(gotGroups) {
		t.Fatalf("Expected %v Got %v", expectedGroups, gotGroups)
	}
}

func TestBackendCreateBalancingMode(t *testing.T) {
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	pool, _ := newTestJig(f, fakeIGs, false)
	nodePort := ServicePort{Port: 8080}
	modes := []BalancingMode{Rate, Utilization}

	// block the creation of Backends with the given balancingMode
	// and verify that a backend with the other balancingMode is
	// created
	for i, bm := range modes {
		f.errFunc = func(op int, be *compute.BackendService) error {
			for _, b := range be.Backends {
				if b.BalancingMode == string(bm) {
					return &googleapi.Error{Code: http.StatusBadRequest}
				}
			}
			return nil
		}

		pool.Ensure([]ServicePort{nodePort}, nil)
		be, err := f.GetGlobalBackendService(defaultNamer.Backend(nodePort.Port))
		if err != nil {
			t.Fatalf("%v", err)
		}

		for _, b := range be.Backends {
			if b.BalancingMode != string(modes[(i+1)%len(modes)]) {
				t.Fatalf("Wrong balancing mode, expected %v got %v", modes[(i+1)%len(modes)], b.BalancingMode)
			}
		}
		pool.GC([]ServicePort{})
	}
}

func TestApplyProbeSettingsToHC(t *testing.T) {
	p := "healthz"
	hc := healthchecks.DefaultHealthCheck(8080, utils.ProtocolHTTPS)
	probe := &api_v1.Probe{
		Handler: api_v1.Handler{
			HTTPGet: &api_v1.HTTPGetAction{
				Scheme: api_v1.URISchemeHTTP,
				Path:   p,
				Port: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: 80,
				},
			},
		},
	}

	applyProbeSettingsToHC(probe, hc)

	if hc.Protocol() != utils.ProtocolHTTPS || hc.Port != 8080 {
		t.Errorf("Basic HC settings changed")
	}
	if hc.RequestPath != "/"+p {
		t.Errorf("Failed to apply probe's requestpath")
	}
}

func TestLinkBackendServiceToNEG(t *testing.T) {
	zones := []string{"zone1", "zone2"}
	namespace, name, port := "ns", "name", "port"
	f := NewFakeBackendServices(noOpErrFunc)
	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), defaultNamer)
	fakeNEG := networkendpointgroup.NewFakeNetworkEndpointGroupCloud("test-subnetwork", "test-network")
	nodePool := instances.NewNodePool(fakeIGs, defaultNamer)
	nodePool.Init(&instances.FakeZoneLister{Zones: []string{defaultZone}})
	hcp := healthchecks.NewFakeHealthCheckProvider()
	healthChecks := healthchecks.NewHealthChecker(hcp, "/", defaultNamer)
	bp := NewBackendPool(f, fakeNEG, healthChecks, nodePool, defaultNamer, []int64{}, false)

	svcPort := ServicePort{
		Port:     30001,
		Protocol: utils.ProtocolHTTP,
		SvcName: types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		},
		SvcPort:       intstr.FromInt(80),
		SvcTargetPort: port,
		NEGEnabled:    true,
	}
	if err := bp.Ensure([]ServicePort{svcPort}, nil); err != nil {
		t.Fatalf("Failed to ensure backend service: %v", err)
	}

	for _, zone := range zones {
		err := fakeNEG.CreateNetworkEndpointGroup(&computealpha.NetworkEndpointGroup{
			Name: defaultNamer.NEG(namespace, name, port),
		}, zone)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if err := bp.Link(svcPort, zones); err != nil {
		t.Fatalf("Failed to link backend service to NEG: %v", err)
	}

	bs, err := f.GetGlobalBackendService(defaultNamer.Backend(svcPort.Port))
	if err != nil {
		t.Fatalf("Failed to retrieve backend service: %v", err)
	}
	if len(bs.Backends) != len(zones) {
		t.Errorf("Expect %v backends, but got %v.", len(zones), len(bs.Backends))
	}

	for _, be := range bs.Backends {
		neg := "NetworkEndpointGroup"
		if !strings.Contains(be.Group, neg) {
			t.Errorf("Expect backend to be a NEG, but got %q", be.Group)
		}
	}
}

func TestRetrieveObjectName(t *testing.T) {
	testCases := []struct {
		url    string
		expect string
	}{
		{
			"",
			"",
		},
		{
			"a/b/c/d/",
			"",
		},
		{
			"a/b/c/d",
			"d",
		},
		{
			"compute",
			"compute",
		},
	}

	for _, tc := range testCases {
		if retrieveObjectName(tc.url) != tc.expect {
			t.Errorf("expect %q, but got %q", tc.expect, retrieveObjectName(tc.url))
		}
	}

}
