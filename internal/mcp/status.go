package mcp

import "sort"

// ConnectionStatus is a non-blocking snapshot of registry-owned connection
// state. It does not ping or reconnect; callers can render status without
// stalling their UI while the background health monitor owns recovery.
type ConnectionStatus struct {
	Name      string
	Connected bool
	ToolCount int
	LastError string
}

// ConnectionStatuses returns one deterministic row per known server. A current
// failed-server receipt wins over a retained client entry until a successful
// reconnect clears it.
func (r *Registry) ConnectionStatuses() []ConnectionStatus {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	byName := make(map[string]ConnectionStatus, len(r.clients)+len(r.failedServers))
	for name := range r.clients {
		byName[name] = ConnectionStatus{
			Name: name, Connected: true, ToolCount: len(r.serverTools[name]),
		}
	}
	for _, failure := range r.failedServers {
		status := byName[failure.Name]
		status.Name = failure.Name
		status.Connected = false
		status.LastError = failure.Reason
		byName[failure.Name] = status
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	statuses := make([]ConnectionStatus, 0, len(names))
	for _, name := range names {
		statuses = append(statuses, byName[name])
	}
	return statuses
}
