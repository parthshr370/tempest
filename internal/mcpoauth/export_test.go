package mcpoauth

// TreatServerAsRemote makes c behave as if its server had only public
// addresses, so tests on loopback can exercise the remote-server policy.
func TreatServerAsRemote(c *Client) {
	c.dest.mu.Lock()
	defer c.dest.mu.Unlock()
	c.dest.resolved, c.dest.classes = true, map[netClass]bool{}
}
