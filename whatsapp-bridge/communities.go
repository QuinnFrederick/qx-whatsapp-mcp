package main

// communities.go — WhatsApp Communities endpoints (fork, see FORK.md).
//
// A community is a parent group (GroupInfo.IsParent). Its groups point at it
// through LinkedParentJID. The announcement group is the sub-group the server
// creates together with the community and flags IsDefaultSubGroup: every
// community member is in it and only admins can post there.
//
// Registered from registerGroupRoutes so main.go keeps its single fork line.
// Same JSON vocabulary and status codes as groups.go.

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// queryBool reads an optional boolean query parameter ("1"/"true"/"0"/"false").
func queryBool(r *http.Request, key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key))) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return def
}

// selfJIDs returns the logged-in account's phone JID and LID (whichever are
// known), so admin status can be read from a participant list in either form.
func selfJIDs(client *whatsmeow.Client) []types.JID {
	var out []types.JID
	if jid := client.Store.GetJID(); !jid.IsEmpty() {
		out = append(out, jid.ToNonAD())
	}
	if lid := client.Store.GetLID(); !lid.IsEmpty() {
		out = append(out, lid.ToNonAD())
	}
	return out
}

// adminStatus reports whether any of self is an admin of the group. It returns
// nil when self does not appear in the participant list at all, which for a
// community parent means WhatsApp did not include us, not that we are absent.
func adminStatus(info *types.GroupInfo, self []types.JID) any {
	for _, p := range info.Participants {
		for _, s := range self {
			if p.JID.ToNonAD() == s || p.LID.ToNonAD() == s || p.PhoneNumber.ToNonAD() == s {
				return p.IsAdmin || p.IsSuperAdmin
			}
		}
	}
	return nil
}

func subGroupJSON(jid types.JID, name string, isAnnouncement, joined bool) map[string]any {
	return map[string]any{
		"jid":                   jid.String(),
		"name":                  name,
		"is_announcement_group": isAnnouncement,
		"joined":                joined,
	}
}

// buildCommunities turns the joined-groups list into one entry per community.
// serverSubGroups holds WhatsApp's full sub-group list per community (it also
// names groups we have not joined); communities missing from it fall back to
// the linked groups we are in, and say so in sub_groups_source.
func buildCommunities(joined []*types.GroupInfo, serverSubGroups map[types.JID][]*types.GroupLinkTarget, self []types.JID) []map[string]any {
	parents := map[types.JID]*types.GroupInfo{}
	linked := map[types.JID][]*types.GroupInfo{}
	joinedSet := map[types.JID]bool{}
	for _, g := range joined {
		if g == nil {
			continue
		}
		joinedSet[g.JID] = true
		switch {
		case g.IsParent:
			parents[g.JID] = g
		case !g.LinkedParentJID.IsEmpty():
			linked[g.LinkedParentJID] = append(linked[g.LinkedParentJID], g)
		}
	}
	// A linked group whose parent was not in the list still proves the
	// community exists; list it with what we know.
	for parent := range linked {
		if _, ok := parents[parent]; !ok {
			parents[parent] = &types.GroupInfo{JID: parent}
		}
	}

	out := make([]map[string]any, 0, len(parents))
	for jid, info := range parents {
		var subs []map[string]any
		source := "server"
		if targets, ok := serverSubGroups[jid]; ok {
			for _, t := range targets {
				subs = append(subs, subGroupJSON(t.JID, t.Name, t.IsDefaultSubGroup, joinedSet[t.JID]))
			}
		} else {
			source = "joined_only"
			for _, g := range linked[jid] {
				subs = append(subs, subGroupJSON(g.JID, g.Name, g.IsDefaultSubGroup, true))
			}
		}
		// Announcement group first, then by name.
		sort.SliceStable(subs, func(i, j int) bool {
			ai, aj := subs[i]["is_announcement_group"].(bool), subs[j]["is_announcement_group"].(bool)
			if ai != aj {
				return ai
			}
			return strings.ToLower(subs[i]["name"].(string)) < strings.ToLower(subs[j]["name"].(string))
		})
		var announcement any
		for _, s := range subs {
			if s["is_announcement_group"].(bool) {
				announcement = map[string]any{"jid": s["jid"], "name": s["name"], "joined": s["joined"]}
				break
			}
		}
		if subs == nil {
			subs = []map[string]any{}
		}
		out = append(out, map[string]any{
			"jid":                info.JID.String(),
			"name":               info.Name,
			"description":        info.Topic,
			"you_are_admin":      adminStatus(info, self),
			"announcement_group": announcement,
			"sub_groups":         subs,
			"sub_groups_source":  source,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i]["name"].(string)) < strings.ToLower(out[j]["name"].(string))
	})
	return out
}

func registerCommunityRoutes(mux *http.ServeMux, client *whatsmeow.Client, auth func(http.HandlerFunc) http.HandlerFunc) {
	// GET /api/communities?sub_groups=1
	//
	// One "participating" IQ for every group we are in, then (unless
	// sub_groups=0) one sub_groups IQ per community for its full group list.
	mux.HandleFunc("/api/communities", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			jsonErr(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		withSubGroups := queryBool(r, "sub_groups", true)
		if !requireClient(w, client) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), groupRequestTimeout)
		defer cancel()
		joined, err := client.GetJoinedGroups(ctx)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "listing joined groups failed: "+err.Error())
			return
		}
		serverSubGroups := map[types.JID][]*types.GroupLinkTarget{}
		warn := map[string]string{}
		if withSubGroups {
			for _, g := range joined {
				if g == nil || !g.IsParent {
					continue
				}
				targets, err := client.GetSubGroups(ctx, g.JID)
				if err != nil {
					warn[g.JID.String()] = "full group list unavailable, showing only groups you are in: " + err.Error()
					continue
				}
				serverSubGroups[g.JID] = targets
			}
		}
		resp := map[string]any{"success": true, "communities": buildCommunities(joined, serverSubGroups, selfJIDs(client))}
		if len(warn) > 0 {
			resp["warnings"] = warn
		}
		writeJSON(w, http.StatusOK, resp)
	}))
}
