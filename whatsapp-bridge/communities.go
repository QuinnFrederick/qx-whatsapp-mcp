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
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type communityGroupsRequest struct {
	CommunityJID string   `json:"community_jid"`
	GroupJIDs    []string `json:"group_jids"`
	Action       string   `json:"action"` // link | unlink
}

type communityCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// newCommunityRequest builds the create IQ for a community: a parent group
// with no participants (members join through its groups or invite link).
// The server creates the announcement group itself. whatsmeow fills in
// default_membership_approval_mode="request_required", the only value seen.
func newCommunityRequest(name string) whatsmeow.ReqCreateGroup {
	return whatsmeow.ReqCreateGroup{Name: name, GroupParent: types.GroupParent{IsParent: true}}
}

// lidResolver is the part of whatsmeow's LID store used to fill in the other
// identity of a member (phone for a LID, LID for a phone).
type lidResolver interface {
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
	GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error)
}

// communityMembersJSON describes each member with both identities where the
// local LID map knows them, in the same shape as a group participant.
// Lookup failures just leave the field null.
func communityMembersJSON(ctx context.Context, members []types.JID, lids lidResolver) []map[string]any {
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		m = m.ToNonAD()
		var pn, lid types.JID
		switch m.Server {
		case types.DefaultUserServer:
			pn = m
			if lids != nil {
				lid, _ = lids.GetLIDForPN(ctx, m)
			}
		case types.HiddenUserServer:
			lid = m
			if lids != nil {
				pn, _ = lids.GetPNForLID(ctx, m)
			}
		}
		out = append(out, map[string]any{"jid": m.String(), "phone": optionalUser(pn), "lid": optionalUser(lid)})
	}
	return out
}

// parseGroupJIDs parses a list of @g.us JIDs, dropping blanks and duplicates.
func parseGroupJIDs(raw []string, field string) ([]types.JID, error) {
	out := make([]types.JID, 0, len(raw))
	seen := map[types.JID]bool{}
	for _, r := range raw {
		if strings.TrimSpace(r) == "" {
			continue
		}
		jid, err := parseGroupJID(r, field)
		if err != nil {
			return nil, fmt.Errorf("%w (got %q)", err, strings.TrimSpace(r))
		}
		if !seen[jid] {
			seen[jid] = true
			out = append(out, jid)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one %s entry is required", field)
	}
	return out, nil
}

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

	// GET /api/community/participants?community_jid=...
	//
	// Everyone in the community's groups (linked_groups_participants IQ), one
	// entry per account.
	mux.HandleFunc("/api/community/participants", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			jsonErr(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		community, err := parseGroupJID(r.URL.Query().Get("community_jid"), "community_jid")
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if !requireClient(w, client) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), groupRequestTimeout)
		defer cancel()
		members, err := client.GetLinkedGroupsParticipants(ctx, community)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "listing community members failed: "+err.Error())
			return
		}
		parts := communityMembersJSON(ctx, members, client.Store.LIDs)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":       true,
			"community_jid": community.String(),
			"count":         len(parts),
			"participants":  parts,
		})
	}))

	// POST /api/community/create {"name", "description"?}
	mux.HandleFunc("/api/community/create", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var req communityCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if err := validateGroupName(req.Name); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if !requireClient(w, client) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), groupRequestTimeout)
		defer cancel()
		info, err := client.CreateGroup(ctx, newCommunityRequest(req.Name))
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "create failed: "+err.Error())
			return
		}
		warn := map[string]string{}
		// Same settle pause as group create before follow-up IQs to the new JID.
		sleepCtx(ctx, postCreateSettleDelay)
		if strings.TrimSpace(req.Description) != "" {
			desc := req.Description
			warn = applySettings(ctx, client, info.JID, nil, &desc, nil, nil, nil, nil)
			if fresh, ferr := fetchGroupInfo(r.Context(), client, info.JID); ferr == nil {
				info = fresh
			} else {
				warn["refresh"] = "community created but re-fetching its info failed: " + ferr.Error()
			}
		}
		resp := map[string]any{"success": true, "community": groupInfoJSON(info), "announcement_group": nil}
		// Best effort and inside the request budget, so create + info fetch +
		// this lookup stay under the server's 60s WriteTimeout.
		subCtx, subCancel := context.WithTimeout(ctx, groupInfoTimeout)
		defer subCancel()
		if targets, serr := client.GetSubGroups(subCtx, info.JID); serr != nil {
			warn["announcement_group"] = "community created but listing its groups failed: " + serr.Error()
		} else {
			for _, t := range targets {
				if t.IsDefaultSubGroup {
					resp["announcement_group"] = map[string]any{"jid": t.JID.String(), "name": t.Name}
				}
			}
		}
		if len(warn) > 0 {
			resp["warnings"] = warn
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	// POST /api/community/groups {"community_jid", "group_jids": [...], "action": "link"|"unlink"}
	//
	// Moves existing groups into or out of a community, one IQ per group, so
	// each gets its own ok flag (like /api/group/participants).
	mux.HandleFunc("/api/community/groups", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var req communityGroupsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		community, err := parseGroupJID(req.CommunityJID, "community_jid")
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		action := strings.ToLower(strings.TrimSpace(req.Action))
		if action != "link" && action != "unlink" {
			jsonErr(w, http.StatusBadRequest, "action must be link or unlink")
			return
		}
		groups, err := parseGroupJIDs(req.GroupJIDs, "group_jids")
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, g := range groups {
			if g == community {
				jsonErr(w, http.StatusBadRequest, "group_jids must not include the community itself")
				return
			}
		}
		if !requireClient(w, client) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), groupRequestTimeout)
		defer cancel()
		results := make([]map[string]any, 0, len(groups))
		allOK := true
		for _, g := range groups {
			if action == "link" {
				err = client.LinkGroup(ctx, community, g)
			} else {
				err = client.UnlinkGroup(ctx, community, g)
			}
			entry := map[string]any{"jid": g.String(), "ok": err == nil}
			if err != nil {
				allOK = false
				entry["error"] = err.Error()
			}
			results = append(results, entry)
		}
		resp := map[string]any{"success": allOK, "action": action, "community_jid": community.String(), "results": results}
		if !allOK {
			resp["message"] = "some groups could not be " + action + "ed; see results (you must be an admin of the community and of each group)"
		}
		writeJSON(w, http.StatusOK, resp)
	}))
}
