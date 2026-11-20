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
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
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

// communityMembersError maps the linked_groups_participants failures seen
// live onto an actionable status: WhatsApp answers 403 forbidden unless the
// account is a community admin.
func communityMembersError(err error) (int, string) {
	switch {
	case errors.Is(err, whatsmeow.ErrIQForbidden):
		return http.StatusForbidden, "only community admins can list a community's members"
	case errors.Is(err, whatsmeow.ErrIQNotFound):
		return http.StatusNotFound, "that community does not exist"
	}
	return http.StatusBadGateway, "listing community members failed: " + err.Error()
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

// communitySubGroup is one group from a community's sub_groups listing.
// whatsmeow's GetSubGroups keeps only jid, subject and default_sub_group, but
// the same response also carries the member count (size) and two flags seen
// live as child elements: <general_chat/> and <hidden_group/> (a group the
// admins hid from the community's group list; only invited members see it).
type communitySubGroup struct {
	JID            types.JID
	Name           string
	Size           int
	IsAnnouncement bool
	IsGeneralChat  bool
	IsHidden       bool
}

// parseSubGroupsNode parses the response to an iq get <sub_groups/>.
func parseSubGroupsNode(res *waBinary.Node) ([]communitySubGroup, error) {
	list, ok := res.GetOptionalChildByTag("sub_groups")
	if !ok {
		return nil, fmt.Errorf("response to sub-groups query has no sub_groups element")
	}
	out := []communitySubGroup{}
	for _, child := range list.GetChildren() {
		if child.Tag != "group" {
			continue
		}
		ag := child.AttrGetter()
		jid := ag.OptionalJIDOrEmpty("jid")
		if jid.IsEmpty() {
			jid = types.NewJID(ag.String("id"), types.GroupServer)
		}
		sg := communitySubGroup{JID: jid, Name: ag.OptionalString("subject"), Size: ag.OptionalInt("size")}
		if !ag.OK() {
			return out, fmt.Errorf("parsing a group in the sub-groups list: %w", ag.Error())
		}
		for _, flag := range child.GetChildren() {
			switch flag.Tag {
			case "default_sub_group":
				sg.IsAnnouncement = true
			case "general_chat":
				sg.IsGeneralChat = true
			case "hidden_group":
				sg.IsHidden = true
			}
		}
		out = append(out, sg)
	}
	return out, nil
}

// fetchSubGroups sends exactly the IQ whatsmeow's GetSubGroups sends (iq get,
// <sub_groups/> to the community) through DangerousInternals, only so that
// parseSubGroupsNode can read the fields GetSubGroups drops. Read-only.
func fetchSubGroups(ctx context.Context, client *whatsmeow.Client, community types.JID) ([]communitySubGroup, error) {
	res, err := client.DangerousInternals().SendGroupIQ(ctx, whatsmeow.DangerousInfoQueryType("get"), community, waBinary.Node{Tag: "sub_groups"})
	if err != nil {
		return nil, err
	}
	return parseSubGroupsNode(res)
}

// subGroupJSON describes one group of a community. hidden, is_general_chat and
// size come only from WhatsApp's full list; for a joined_only entry hidden and
// is_general_chat are null (unknown) and size is the joined group's own count.
func subGroupJSON(jid types.JID, name string, isAnnouncement, joined bool, hidden, generalChat, size any) map[string]any {
	return map[string]any{
		"jid":                   jid.String(),
		"name":                  name,
		"is_announcement_group": isAnnouncement,
		"joined":                joined,
		"hidden":                hidden,
		"is_general_chat":       generalChat,
		"size":                  size,
	}
}

func optionalCount(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}

// buildCommunities turns the joined-groups list into one entry per community.
// serverSubGroups holds WhatsApp's full sub-group list per community (it also
// names groups we have not joined); communities missing from it fall back to
// the linked groups we are in, and say so in sub_groups_source.
func buildCommunities(joined []*types.GroupInfo, serverSubGroups map[types.JID][]communitySubGroup, self []types.JID) []map[string]any {
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
				subs = append(subs, subGroupJSON(t.JID, t.Name, t.IsAnnouncement, joinedSet[t.JID], t.IsHidden, t.IsGeneralChat, optionalCount(t.Size)))
			}
		} else {
			source = "joined_only"
			for _, g := range linked[jid] {
				size := g.ParticipantCount
				if size == 0 {
					size = len(g.Participants)
				}
				subs = append(subs, subGroupJSON(g.JID, g.Name, g.IsDefaultSubGroup, true, nil, nil, optionalCount(size)))
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

// collectSubGroups fetches each community's full group list. A failure is a
// per-community warning; after a 429 rate-overlimit the remaining communities
// are skipped rather than queried, since every further IQ would be refused too
// and only prolongs the limit.
func collectSubGroups[T any](parents []types.JID, fetch func(types.JID) (T, error)) (map[types.JID]T, map[string]string) {
	got := map[types.JID]T{}
	warn := map[string]string{}
	rateLimited := false
	for _, jid := range parents {
		if rateLimited {
			warn[jid.String()] = "full group list skipped: WhatsApp is rate-limiting these queries; ask for this community on its own later"
			continue
		}
		v, err := fetch(jid)
		if err != nil {
			rateLimited = errors.Is(err, whatsmeow.ErrIQRateOverLimit)
			warn[jid.String()] = "full group list unavailable, showing only groups you are in: " + err.Error()
			continue
		}
		got[jid] = v
	}
	return got, warn
}

// filterCommunities keeps only the community with the given JID; an empty JID
// keeps them all.
func filterCommunities(all []map[string]any, only types.JID) []map[string]any {
	if only.IsEmpty() {
		return all
	}
	for _, c := range all {
		if c["jid"] == only.String() {
			return []map[string]any{c}
		}
	}
	return []map[string]any{}
}

func registerCommunityRoutes(mux *http.ServeMux, client *whatsmeow.Client, auth func(http.HandlerFunc) http.HandlerFunc) {
	// GET /api/communities?sub_groups=1
	//
	// GET /api/communities?community_jid=...&sub_groups=0|1
	//
	// One "participating" IQ lists every group we are in, which is enough for
	// an overview (each community with the groups we joined). The full group
	// list costs one sub_groups IQ per community, and WhatsApp answers a burst
	// of those with 429 rate-overlimit (seen live after nine), so it is only
	// fetched by default when a single community_jid is asked for; sub_groups=1
	// fetches it for every community and stops at the first 429.
	mux.HandleFunc("/api/communities", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			jsonErr(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		var only types.JID
		if raw := r.URL.Query().Get("community_jid"); strings.TrimSpace(raw) != "" {
			var err error
			if only, err = parseGroupJID(raw, "community_jid"); err != nil {
				jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		withSubGroups := queryBool(r, "sub_groups", !only.IsEmpty())
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
		serverSubGroups := map[types.JID][]communitySubGroup{}
		warn := map[string]string{}
		if withSubGroups {
			var parents []types.JID
			for _, g := range joined {
				if g != nil && g.IsParent && (only.IsEmpty() || g.JID == only) {
					parents = append(parents, g.JID)
				}
			}
			serverSubGroups, warn = collectSubGroups(parents, func(jid types.JID) ([]communitySubGroup, error) {
				return fetchSubGroups(ctx, client, jid)
			})
		}
		communities := filterCommunities(buildCommunities(joined, serverSubGroups, selfJIDs(client)), only)
		if !only.IsEmpty() && len(communities) == 0 {
			jsonErr(w, http.StatusNotFound, "you are not in a community with that jid")
			return
		}
		resp := map[string]any{"success": true, "communities": communities}
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
			status, msg := communityMembersError(err)
			jsonErr(w, status, msg)
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
