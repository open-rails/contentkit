package hub

// SignalPlane reports whether the hub's signal plane is up.
func (h *Hub) SignalPlane() bool { return h.store.Load() != nil }
