package opensdk

import "context"

// ── the institution's dashboard (M6.21 K2) ──────────────────────────────────

// GetAllInsights reads every page of the institution's dashboard the API offers (GET /insights,
// insights:readonly): the default page first, then each other tab it lists in "tabs", with the same
// query (range, from/to, compare, venue_id, course_id, currency; a "tab" in query is ignored). It
// returns tab → page. Fewer than five people come back as null with "withheld": true; money is per
// currency, never converted.
func (c *Client) GetAllInsights(ctx context.Context, tenantKey string, query Params) (map[string]Object, error) {
	base := Params{}
	for k, v := range query {
		if k != "tab" {
			base[k] = v
		}
	}
	first, err := c.GetInsights(ctx, base, WithTenantKey(tenantKey))
	if err != nil {
		return nil, err
	}
	tab, _ := first["tab"].(string)
	out := map[string]Object{tab: first}
	tabs, _ := first["tabs"].([]any)
	for _, t := range tabs {
		name, _ := t.(string)
		if name == "" {
			continue
		}
		if _, done := out[name]; done {
			continue
		}
		q := Params{"tab": name}
		for k, v := range base {
			q[k] = v
		}
		page, err := c.GetInsights(ctx, q, WithTenantKey(tenantKey))
		if err != nil {
			return nil, err
		}
		out[name] = page
	}
	return out, nil
}
