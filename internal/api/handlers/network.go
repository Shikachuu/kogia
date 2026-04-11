package handlers

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/Shikachuu/kogia/internal/network"
	"github.com/Shikachuu/kogia/internal/store"
	mobynetwork "github.com/moby/moby/api/types/network"
)

// NetworkCreate handles POST /networks/create.
func (h *Handlers) NetworkCreate(w http.ResponseWriter, r *http.Request) {
	var req mobynetwork.CreateRequest

	if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
		respondError(w, errdefs.InvalidParameter("invalid network create request", decErr))

		return
	}

	if req.Name == "" {
		respondError(w, errdefs.InvalidParameter("network name is required", nil))

		return
	}

	// Extract subnet/gateway from IPAM config.
	var subnet netip.Prefix

	var gateway netip.Addr

	if req.IPAM != nil && len(req.IPAM.Config) > 0 {
		subnet = req.IPAM.Config[0].Subnet
		gateway = req.IPAM.Config[0].Gateway
	}

	id, createErr := h.network.CreateNetwork(req.Name, req.Driver, subnet, gateway, req.Internal, req.Options, req.Labels)
	if createErr != nil {
		if errors.Is(createErr, store.ErrNetworkNameInUse) {
			respondError(w, errdefs.Conflict("network with name "+req.Name+" already exists", createErr))

			return
		}

		respondError(w, createErr)

		return
	}

	respondJSON(w, http.StatusCreated, mobynetwork.CreateResponse{
		ID:      id,
		Warning: "",
	})
}

// NetworkDelete handles DELETE /networks/{id}.
func (h *Handlers) NetworkDelete(w http.ResponseWriter, r *http.Request) {
	idOrName := pathValue(r, "id")

	if removeErr := h.network.RemoveNetwork(idOrName); removeErr != nil {
		switch {
		case errors.Is(removeErr, network.ErrNetworkNotFound):
			respondError(w, errdefs.NotFound("network "+idOrName+" not found", removeErr))
		case errors.Is(removeErr, network.ErrPredefinedNetwork):
			respondError(w, errdefs.Conflict(removeErr.Error(), removeErr))
		case errors.Is(removeErr, network.ErrNetworkHasContainers):
			respondError(w, errdefs.Conflict(removeErr.Error(), removeErr))
		default:
			respondError(w, removeErr)
		}

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// NetworkInspect handles GET /networks/{id}.
func (h *Handlers) NetworkInspect(w http.ResponseWriter, r *http.Request) {
	idOrName := pathValue(r, "id")

	rec, getErr := h.network.GetNetwork(idOrName)
	if getErr != nil {
		if isNetworkNotFound(getErr) {
			respondError(w, errdefs.NotFound("network "+idOrName+" not found", getErr))

			return
		}

		respondError(w, getErr)

		return
	}

	respondJSON(w, http.StatusOK, recordToInspect(rec, h.network))
}

// NetworkList handles GET /networks.
func (h *Handlers) NetworkList(w http.ResponseWriter, _ *http.Request) {
	records, listErr := h.network.ListNetworks()
	if listErr != nil {
		respondError(w, listErr)

		return
	}

	nets := make([]mobynetwork.Network, 0, len(records))
	for _, rec := range records {
		nets = append(nets, recordToNetwork(rec))
	}

	respondJSON(w, http.StatusOK, nets)
}

// NetworkConnect handles POST /networks/{id}/connect.
func (h *Handlers) NetworkConnect(w http.ResponseWriter, r *http.Request) {
	networkID := pathValue(r, "id")

	var req mobynetwork.ConnectRequest

	if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
		respondError(w, errdefs.InvalidParameter("invalid connect request", decErr))

		return
	}

	if req.Container == "" {
		respondError(w, errdefs.InvalidParameter("container ID is required", nil))

		return
	}

	// Look up the container to get its PID.
	record, containerErr := h.store.GetContainer(req.Container)
	if containerErr != nil {
		respondError(w, errdefs.NotFound("container "+req.Container+" not found", containerErr))

		return
	}

	if record.State == nil || !record.State.Running {
		respondError(w, errdefs.Conflict("container "+req.Container+" is not running", nil))

		return
	}

	// Resolve network ID.
	netRec, netErr := h.network.GetNetwork(networkID)
	if netErr != nil {
		respondError(w, errdefs.NotFound("network "+networkID+" not found", netErr))

		return
	}

	if _, connectErr := h.network.Connect(netRec.ID, record.ID, record.Name, record.State.Pid, nil); connectErr != nil {
		respondError(w, connectErr)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// NetworkDisconnect handles POST /networks/{id}/disconnect.
func (h *Handlers) NetworkDisconnect(w http.ResponseWriter, r *http.Request) {
	networkID := pathValue(r, "id")

	var req mobynetwork.DisconnectRequest

	if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
		respondError(w, errdefs.InvalidParameter("invalid disconnect request", decErr))

		return
	}

	if req.Container == "" {
		respondError(w, errdefs.InvalidParameter("container ID is required", nil))

		return
	}

	// Resolve network ID.
	netRec, netErr := h.network.GetNetwork(networkID)
	if netErr != nil {
		respondError(w, errdefs.NotFound("network "+networkID+" not found", netErr))

		return
	}

	if disconnErr := h.network.Disconnect(netRec.ID, req.Container, req.Force); disconnErr != nil {
		respondError(w, disconnErr)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// NetworkPrune handles POST /networks/prune.
func (h *Handlers) NetworkPrune(w http.ResponseWriter, _ *http.Request) {
	records, listErr := h.network.ListNetworks()
	if listErr != nil {
		respondError(w, listErr)

		return
	}

	var deleted []string

	for _, rec := range records {
		// Skip predefined networks.
		if rec.Name == network.DefaultNetworkName || rec.Driver == "host" || rec.Driver == "none" {
			continue
		}

		endpoints, epErr := h.network.ListEndpoints(rec.ID)
		if epErr != nil || len(endpoints) > 0 {
			continue
		}

		if rmErr := h.network.RemoveNetwork(rec.ID); rmErr == nil {
			deleted = append(deleted, rec.Name)
		}
	}

	respondJSON(w, http.StatusOK, mobynetwork.PruneReport{
		NetworksDeleted: deleted,
	})
}

// recordToNetwork converts an internal network.Record to the Docker API Network type.
func recordToNetwork(rec *network.Record) mobynetwork.Network {
	n := mobynetwork.Network{
		Name:       rec.Name,
		ID:         rec.ID,
		Created:    rec.Created,
		Scope:      "local",
		Driver:     rec.Driver,
		EnableIPv4: true,
		Internal:   rec.Internal,
		Options:    rec.Options,
		Labels:     rec.Labels,
	}

	if rec.Subnet.IsValid() {
		n.IPAM = mobynetwork.IPAM{
			Driver: "default",
			Config: []mobynetwork.IPAMConfig{
				{
					Subnet:  rec.Subnet,
					Gateway: rec.Gateway,
				},
			},
		}
	}

	return n
}

// recordToInspect converts an internal network.Record to the Docker API Inspect type
// (includes connected container endpoints).
func recordToInspect(rec *network.Record, mgr *network.Manager) mobynetwork.Inspect {
	inspect := mobynetwork.Inspect{
		Network: recordToNetwork(rec),
	}

	if mgr != nil {
		endpoints, epErr := mgr.ListEndpoints(rec.ID)
		if epErr == nil && len(endpoints) > 0 {
			containers := make(map[string]mobynetwork.EndpointResource, len(endpoints))

			for _, ep := range endpoints {
				mac, _ := net.ParseMAC(ep.MacAddress)

				containers[ep.ContainerID] = mobynetwork.EndpointResource{
					Name:        ep.ContainerName,
					IPv4Address: netip.PrefixFrom(ep.IPAddress, rec.Subnet.Bits()),
					MacAddress:  mobynetwork.HardwareAddr(mac),
				}
			}

			inspect.Containers = containers
		}
	}

	return inspect
}

func isNetworkNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound) || errors.Is(err, network.ErrNetworkNotFound)
}
