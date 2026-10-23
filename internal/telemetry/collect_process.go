package telemetry

func (m *Manager) collectProcess(c *collection) {
	pid := m.source.PID()
	if pid <= 0 {
		return
	}
	var proc processData
	c.attempt("jvm_process", func() ([]point, error) {
		var err error
		proc, err = readProcess("/proc", pid, m.clockTicks, m.bootTime)
		if err != nil {
			return nil, err
		}
		if err = verifyJavaProcess("/proc", pid); err != nil {
			return nil, err
		}
		c.PID, c.ProcessStart = pid, proc.StartTicks
		return proc.points("mcs_jvm_process"), nil
	})
	if !c.Health["jvm_process"].Success {
		return
	}
	c.attempt("jvm_perfdata", func() ([]point, error) {
		perf, err := readPerfData("/proc", pid, proc)
		if err != nil {
			return nil, err
		}
		return perf.points(), nil
	})
	// Discard both sets if systemd changed its PID during collection.
	if m.source.PID() != pid {
		for _, name := range []string{"jvm_process", "jvm_perfdata"} {
			c.Points[name] = nil
			h := c.Health[name]
			h.Success = false
			c.Health[name] = h
		}
	}
}
