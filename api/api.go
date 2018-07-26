// Package api implements the oftee API that can be used for injecting
// packets to the open flow devices
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ciena/oftee/injector"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// MappingAction defines DPID mapping actions
type MappingAction uint8

const (
	// MapActionNone no op action
	MapActionNone MappingAction = 0x0
	// MapActionAdd indicates addition of mapping
	MapActionAdd MappingAction = 1 << 0
	// MapActionDelete indicated deletion of mapping
	MapActionDelete MappingAction = 1 << 1
)

// DPIDMapping is used to associate a DPID with an injecting packet processor
type DPIDMapping struct {
	Action MappingAction
	DPID   uint64
	Inject injector.Injector
}

// API maintains the configuration and runtime information for the API
type API struct {
	DPIDMappingListener chan DPIDMapping
	ListenOn            string

	injectors map[uint64]injector.Injector
	router    *mux.Router
	serveMux  *http.ServeMux
	lock      sync.RWMutex
}

// DevicesResponse is used to create a HTTP response that lists all the known DPIDs
type DevicesResponse struct {
	Devices []string `json:"devices"`
}

// ListDevicesHandler returns a list of DPIDs known to the system as a JSON array
func (api *API) ListDevicesHandler(resp http.ResponseWriter, req *http.Request) {

	// Create the response object
	api.lock.RLock()
	data := DevicesResponse{
		Devices: make([]string, len(api.injectors)),
	}
	i := 0
	for key := range api.injectors {
		data.Devices[i] = fmt.Sprintf("of:0x%016x", key)
		i++
	}
	api.lock.RUnlock()

	// Convert it to bytes and return it
	bytes, err := json.Marshal(data)
	if err != nil {
		http.Error(resp,
			fmt.Sprintf("Unable to marshal device list : %s", err.Error()),
			http.StatusInternalServerError)
		return
	}
	_, err = resp.Write(bytes)
	if err != nil {
		log.
			WithError(err).
			Error("Unable to write device list to HTTP response")
	}
}

// PacketOutHandler handles an HTTP request to packet out to a given switch port. The payload to
// the request should be the []byte of a OpenFlow packet out message, including
// the open flow header, the packet out header, and the packet.
func (api *API) PacketOutHandler(resp http.ResponseWriter, req *http.Request) {
	defer api.close(req.Body)

	// Parse the URL for the target device's DPID
	vars := mux.Vars(req)
	log.WithFields(log.Fields{
		"dpid": vars["dpid"],
	}).Debug("Packet out request recieved")
	dpid, err := strconv.ParseUint(vars["dpid"], 0, 64)
	if err != nil {
		log.WithFields(log.Fields{
			"dpid": vars["dpid"],
		}).Warn("Unable to parse given DPID")
		http.Error(resp, fmt.Sprintf("DPID doesn't reference a device, '%s' : %s", vars["dpid"], err), http.StatusNotFound)
		return
	}
	api.lock.RLock()
	inject, ok := api.injectors[uint64(dpid)]
	api.lock.RUnlock()

	// If DPID doesn't exist in mapping, then 404
	if !ok {
		log.WithFields(log.Fields{
			"dpid": vars["dpid"],
		}).Warn("Unable to find packet injector for DPID, unknown device")
		http.Error(resp, fmt.Sprintf("DPID not found, '%s'", vars["dpid"]), http.StatusNotFound)
		return
	}

	// Read the OpenFlow message from the body
	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return
	}

	// Inject the packet
	inject.Inject(data)
}

// close wraps an io.Closer.Close call so that any error can be logged
func (api *API) close(c io.Closer) {
	if err := c.Close(); err != nil {
		log.
			WithError(err).
			Error("Error when attempting to close packet out request response")
	}
}

// Loop that listens for updates of DPID mappings
func (api *API) dpidMappingUpdates() {
	for {
		mapping := <-api.DPIDMappingListener

		switch mapping.Action {
		case MapActionAdd:
			log.WithFields(log.Fields{
				"dpid": fmt.Sprintf("0x%016x", mapping.DPID),
			}).Debug("Adding device mapping")
			api.lock.Lock()
			api.injectors[mapping.DPID] = mapping.Inject
			api.lock.Unlock()
		case MapActionDelete:
			log.WithFields(log.Fields{
				"dpid": fmt.Sprintf("0x%016x", mapping.DPID),
			}).Debug("Deleting device mapping")
			api.lock.Lock()
			delete(api.injectors, mapping.DPID)
			api.lock.Unlock()
		default:
			log.WithFields(log.Fields{
				"dpid":   fmt.Sprintf("0x%016x", mapping.DPID),
				"action": mapping.Action,
			}).Warn("Received unknown device mapping action")
		}
	}
}

// NewAPI properly instantiates a new API instance.
func NewAPI(listenOn string) *API {
	api := &API{
		ListenOn:            listenOn,
		router:              mux.NewRouter(),
		serveMux:            http.NewServeMux(),
		injectors:           make(map[uint64]injector.Injector),
		DPIDMappingListener: make(chan DPIDMapping),
	}

	api.router.
		HandleFunc("/oftee/{dpid}", api.PacketOutHandler).
		Methods("POST").
		Headers("Content-type", "application/octet-stream")
	api.router.
		HandleFunc("/oftee", api.ListDevicesHandler).
		Methods("GET")
	api.serveMux.Handle("/", api.router)
	return api
}

// ListenAndServe implements the API service loop
func (api *API) ListenAndServe() {

	// TODO It is good Go practice to handle structures that arrive
	// "unitialized". This should be done here, so NewAPI does not "have" to
	// be called.

	srv := &http.Server{
		Addr:    api.ListenOn,
		Handler: api.serveMux,
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	// Start the DPID update listener
	log.Debug("Start API listening for device DPID information")
	go api.dpidMappingUpdates()

	log.WithFields(log.Fields{
		"connect-point": api.ListenOn,
	}).Debug("Listening for REST API requests")
	log.Fatal(srv.ListenAndServe())
}
