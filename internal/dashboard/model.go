package dashboard

func (m *Model) Observe(snapshot LiveSnapshot, runID string) {
	m.Live = &snapshot
	if snapshot.Err != nil {
		m.Connection, m.Message = "disconnected; history available", snapshot.Err.Error()
	} else if snapshot.Info.RunID != runID {
		m.Connection, m.Message = "new run available", "Router restarted. Press r to attach to the new run; current history stays open."
	} else {
		m.Connection, m.Message = "connected", ""
	}
}

type Model struct {
	Selected     int64
	Follow       bool
	RouteFocus   bool
	RouteOffset  int
	Details      bool
	DetailOffset int
	Live         *LiveSnapshot
	Connection   string
	Message      string
}

func NewModel() *Model { return &Model{Selected: -1, Follow: true, Connection: "saved run"} }

func (m *Model) Sync(count int64) {
	if count == 0 {
		m.Selected = -1
		return
	}
	if m.Follow || m.Selected >= count {
		m.Selected = count - 1
	}
}

// Key returns an action for the controller; browsing does not touch the router.
func (m *Model) Key(key string, count int64, height int) string {
	step := 1
	switch key {
	case "quit":
		return "quit"
	case "reconnect":
		return "reconnect"
	case "escape":
		m.Details = false
		m.DetailOffset = 0
		return ""
	case "enter":
		if m.Selected >= 0 {
			m.Details = !m.Details
			m.DetailOffset = 0
		}
		return ""
	case "tab":
		if !m.Details {
			m.RouteFocus = !m.RouteFocus
		}
		return ""
	case "pause":
		m.Follow = !m.Follow
		m.Sync(count)
		return ""
	case "end":
		m.Follow = true
		m.Details = false
		m.Sync(count)
		return ""
	case "home":
		m.Selected = 0
		if count == 0 {
			m.Selected = -1
		}
		m.Follow = false
		m.DetailOffset = 0
		return ""
	case "pageup", "pagedown":
		step = max(1, height/2)
	case "wheelup", "wheeldown":
		step = 3
	}
	direction := 0
	switch key {
	case "up", "pageup", "wheelup":
		direction = -1
	case "down", "pagedown", "wheeldown":
		direction = 1
	}
	if direction == 0 {
		return ""
	}
	if m.Details {
		m.DetailOffset = max(0, m.DetailOffset+direction*step)
		return ""
	}
	if m.RouteFocus {
		m.RouteOffset = max(0, m.RouteOffset+direction*step)
		return ""
	}
	if count > 0 {
		m.Selected = max(int64(0), min(count-1, m.Selected+int64(direction*step)))
	}
	m.Follow = false
	return ""
}
