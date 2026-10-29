// SPDX-License-Identifier: MIT

package ocapi

import "context"

// Locations lists cached service graphs, not projects or persisted sessions.
func (c *Client) Locations(ctx context.Context) ([]Location, error) {
	r := capped[Location]{max: 10000}
	err := c.json(ctx, "GET", "/api/debug/location", nil, nil, &r)
	return r.v, err
}

// EvictLocation disposes cached services without deleting restorable chats.
func (c *Client) EvictLocation(ctx context.Context, dir string) error {
	return c.json(ctx, "DELETE", "/api/debug/location", loc(dir), nil, nil)
}

// LocationReload rebuilds every loaded location: skills, MCP connections and
// tool lists are read again. Pending permissions and forms are cancelled, so
// callers only use it while no session is active. Measured against 2.0.30 in
// the runtime image: 204 after ~0.2 s; for about a second afterwards
// /api/skill answers with the built-in skills only.
func (c *Client) LocationReload(ctx context.Context) error {
	return c.json(ctx, "POST", "/api/location/reload", nil, nil, nil)
}
