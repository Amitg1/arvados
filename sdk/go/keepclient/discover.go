package keepclient

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"
)

// DiscoverKeepServers gets list of available keep services from the
// API server.
//
// If a list of services is provided in the arvadosclient (e.g., from
// an environment variable or local config), that list is used
// instead.
func (this *KeepClient) DiscoverKeepServers() error {
	if this.Arvados.KeepServiceURIs != nil {
		this.foundNonDiskSvc = true
		this.replicasPerService = 0
		roots := make(map[string]string)
		for i, uri := range this.Arvados.KeepServiceURIs {
			roots[fmt.Sprintf("00000-bi6l4-%015d", i)] = uri
		}
		this.SetServiceRoots(roots, roots, roots)
		return nil
	}

	// ArvadosClient did not provide a services list. Ask API
	// server for a list of accessible services.
	var list svcList
	err := this.Arvados.Call("GET", "keep_services", "", "accessible", nil, &list)
	if err != nil {
		return err
	}
	return this.loadKeepServers(list)
}

// LoadKeepServicesFromJSON gets list of available keep services from given JSON
func (this *KeepClient) LoadKeepServicesFromJSON(services string) error {
	var list svcList

	// Load keep services from given json
	dec := json.NewDecoder(strings.NewReader(services))
	if err := dec.Decode(&list); err != nil {
		return err
	}

	return this.loadKeepServers(list)
}

// RefreshServices calls DiscoverKeepServers to refresh the keep
// service list on SIGHUP; when the given interval has elapsed since
// the last refresh; and (if the last refresh failed) the given
// errInterval has elapsed.
func (kc *KeepClient) RefreshServices(interval, errInterval time.Duration) {
	var previousRoots = []map[string]string{}

	timer := time.NewTimer(interval)
	gotHUP := make(chan os.Signal, 1)
	signal.Notify(gotHUP, syscall.SIGHUP)

	for {
		select {
		case <-gotHUP:
		case <-timer.C:
		}
		timer.Reset(interval)

		if err := kc.DiscoverKeepServers(); err != nil {
			log.Printf("WARNING: Error retrieving services list: %v (retrying in %v)", err, errInterval)
			timer.Reset(errInterval)
			continue
		}
		newRoots := []map[string]string{kc.LocalRoots(), kc.GatewayRoots()}

		if !reflect.DeepEqual(previousRoots, newRoots) {
			DebugPrintf("DEBUG: Updated services list: locals %v gateways %v", newRoots[0], newRoots[1])
			previousRoots = newRoots
		}

		if len(newRoots[0]) == 0 {
			log.Printf("WARNING: No local services (retrying in %v)", errInterval)
			timer.Reset(errInterval)
		}
	}
}

// loadKeepServers
func (this *KeepClient) loadKeepServers(list svcList) error {
	listed := make(map[string]bool)
	localRoots := make(map[string]string)
	gatewayRoots := make(map[string]string)
	writableLocalRoots := make(map[string]string)

	// replicasPerService is 1 for disks; unknown or unlimited otherwise
	this.replicasPerService = 1

	for _, service := range list.Items {
		scheme := "http"
		if service.SSL {
			scheme = "https"
		}
		url := fmt.Sprintf("%s://%s:%d", scheme, service.Hostname, service.Port)

		// Skip duplicates
		if listed[url] {
			continue
		}
		listed[url] = true

		localRoots[service.Uuid] = url
		if service.ReadOnly == false {
			writableLocalRoots[service.Uuid] = url
			if service.SvcType != "disk" {
				this.replicasPerService = 0
			}
		}

		if service.SvcType != "disk" {
			this.foundNonDiskSvc = true
		}

		// Gateway services are only used when specified by
		// UUID, so there's nothing to gain by filtering them
		// by service type. Including all accessible services
		// (gateway and otherwise) merely accommodates more
		// service configurations.
		gatewayRoots[service.Uuid] = url
	}

	this.SetServiceRoots(localRoots, writableLocalRoots, gatewayRoots)
	return nil
}
