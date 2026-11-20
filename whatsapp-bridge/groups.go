package main

// groups.go — group management and profile-picture endpoints.
//
// Added in the QuinnFrederick fork (2026-10-09). Kept in its own file so the
// diff against upstream verygoodplugins/whatsapp-mcp stays a single line in
// main.go (the registerGroupRoutes call). See FORK.md for the HTTP contract.
//
// Everything here is a thin wrapper over whatsmeow's group primitives
// (CreateGroup, SetGroup*, UpdateGroupParticipants, GetProfilePictureInfo).
// Responses use the same {"success": bool, "message": string} vocabulary as
// the upstream /api/send and /api/download handlers, but always as JSON so an
// MCP client can surface per-participant failures instead of a bare 4xx.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	// maxGroupNameRunes is WhatsApp's group subject limit, counted in
	// characters (code points), not bytes. WhatsApp raised it from 25 to 100 in
	// 2023; whatsmeow's ReqCreateGroup comment still says 25. The server rejects
	// an over-long subject (406 not-acceptable), so fail fast with a useful
	// message instead. (The Business Cloud API allows 128; that limit does not
	// apply to a linked consumer device like this bridge.)
	maxGroupNameRunes = 100
	// postCreateSettleDelay is the pause between a successful create and any
	// follow-up IQs (description, member-add mode) sent to the new group JID.
	// Immediately after the create response the server has been seen to
	// answer those with item-not-found. It is only paid when there is
	// something to apply, and it respects request cancellation.
	postCreateSettleDelay = 1500 * time.Millisecond
	// groupRequestTimeout bounds all the WhatsApp round-trips of one request. It
	// must stay well under the HTTP server's WriteTimeout (60s in main.go): the
	// server has been seen to never answer a malformed description IQ, and if
	// the handler is still waiting when WriteTimeout fires the connection is
	// simply dropped and the client sees a reset instead of a warning.
	groupRequestTimeout = 40 * time.Second
	// groupInfoTimeout is a separate budget for the final GetGroupInfo so the
	// response still carries the group's real state after a slow setting.
	groupInfoTimeout      = 10 * time.Second
	pictureRequestTimeout = 20 * time.Second
)

type groupCreateRequest struct {
	Name         string   `json:"name"`
	Participants []string `json:"participants"`
	Description  string   `json:"description,omitempty"`
	// Settings (all optional; omitted = WhatsApp default)
	Announce     *bool   `json:"announce,omitempty"`      // only admins can send
	Locked       *bool   `json:"locked,omitempty"`        // only admins can edit group info
	MemberAdd    *string `json:"member_add,omitempty"`    // "all_members" | "admins_only"
	JoinApproval *bool   `json:"join_approval,omitempty"` // admins must approve joiners
	Ephemeral    *uint32 `json:"ephemeral_seconds,omitempty"`
}

type groupSettingsRequest struct {
	GroupJID     string  `json:"group_jid"`
	Name         *string `json:"name,omitempty"`
	Description  *string `json:"description,omitempty"`
	Announce     *bool   `json:"announce,omitempty"`
	Locked       *bool   `json:"locked,omitempty"`
	MemberAdd    *string `json:"member_add,omitempty"`
	JoinApproval *bool   `json:"join_approval,omitempty"`
}

type groupParticipantsRequest struct {
	GroupJID     string   `json:"group_jid"`
	Participants []string `json:"participants"`
	Action       string   `json:"action"` // add | remove | promote | demote
}

// parseJIDOrPhone accepts a full JID (user, LID or group) or a bare phone
// number in any common formatting ("+1 (202) 555-1234"). Phone numbers become
// @s.whatsapp.net JIDs; device suffixes ("…:12@s.whatsapp.net") are dropped
// because group operations address accounts, not devices.
func parseJIDOrPhone(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.JID{}, fmt.Errorf("jid or phone number is required")
	}
	if strings.Contains(raw, "@") {
		jid, err := types.ParseJID(raw)
		if err != nil {
			return types.JID{}, fmt.Errorf("invalid JID %q: %w", raw, err)
		}
		return jid.ToNonAD(), nil
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, raw)
	if digits == "" {
		return types.JID{}, fmt.Errorf("invalid participant %q: expected a phone number or JID", raw)
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

// parseUserJIDs maps participant entries to account JIDs, dropping blanks and
// duplicates. Group JIDs are rejected because they can never be participants.
func parseUserJIDs(raw []string) ([]types.JID, error) {
	out := make([]types.JID, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, p := range raw {
		if strings.TrimSpace(p) == "" {
			continue
		}
		jid, err := parseJIDOrPhone(p)
		if err != nil {
			return nil, err
		}
		if jid.Server == types.GroupServer {
			return nil, fmt.Errorf("participant %q is a group JID", strings.TrimSpace(p))
		}
		key := jid.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, jid)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one participant is required")
	}
	return out, nil
}

func memberAddMode(s string) (types.GroupMemberAddMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "all_members", "all", "everyone":
		return types.GroupMemberAddModeAllMember, nil
	case "admins_only", "admins", "admin":
		return types.GroupMemberAddModeAdmin, nil
	}
	return "", fmt.Errorf("member_add must be all_members or admins_only")
}

// memberAddModeString maps whatsmeow's wire values back onto the vocabulary
// the request side accepts, so a client can round-trip what it reads.
func memberAddModeString(m types.GroupMemberAddMode) string {
	switch m {
	case types.GroupMemberAddModeAdmin:
		return "admins_only"
	case types.GroupMemberAddModeAllMember:
		return "all_members"
	}
	return string(m)
}

// validateGroupName returns a user-facing error for an unusable subject.
func validateGroupName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len([]rune(name)) > maxGroupNameRunes {
		return fmt.Errorf("name must be %d characters or fewer", maxGroupNameRunes)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "message": msg})
}

// requireClient rejects the request when the bridge is not logged in, so a
// disconnected bridge reads as 503 rather than an opaque 502 from whatsmeow.
// Call it after request validation so bad input is still reported as 400.
func requireClient(w http.ResponseWriter, client *whatsmeow.Client) bool {
	if client == nil || !client.IsLoggedIn() {
		jsonErr(w, http.StatusServiceUnavailable, "WhatsApp client is not logged in")
		return false
	}
	return true
}

// sleepCtx waits for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// participantErrorHint translates WhatsApp's per-participant status codes into
// something a caller can act on. Unknown codes return "" and only the number
// is reported.
func participantErrorHint(code int) string {
	switch code {
	case 403:
		return "privacy settings prevent adding this user directly; if add_request is present, send them that invite code"
	case 408:
		return "user recently left the group and cannot be re-added yet"
	case 409:
		return "user is already in the group"
	case 500:
		return "server error"
	}
	return ""
}

func optionalUser(j types.JID) any {
	if j.IsEmpty() {
		return nil
	}
	return j.User
}

func participantJSON(p types.GroupParticipant) map[string]any {
	m := map[string]any{
		"jid":            p.JID.String(),
		"phone":          optionalUser(p.PhoneNumber),
		"lid":            optionalUser(p.LID),
		"is_admin":       p.IsAdmin,
		"is_super_admin": p.IsSuperAdmin,
	}
	if p.Error != 0 {
		m["error"] = p.Error
		if hint := participantErrorHint(p.Error); hint != "" {
			m["error_hint"] = hint
		}
		if p.AddRequest != nil {
			m["add_request"] = map[string]any{
				"code":       p.AddRequest.Code,
				"expiration": p.AddRequest.Expiration.Unix(),
			}
		}
	}
	return m
}

// failedParticipants extracts the entries WhatsApp refused. CreateGroup still
// succeeds in that case, so without this the caller would silently get a
// smaller group than asked for.
func failedParticipants(parts []types.GroupParticipant) []map[string]any {
	var failed []map[string]any
	for _, p := range parts {
		if p.Error != 0 {
			failed = append(failed, participantJSON(p))
		}
	}
	return failed
}

func groupInfoJSON(info *types.GroupInfo) map[string]any {
	parts := make([]map[string]any, 0, len(info.Participants))
	for _, p := range info.Participants {
		parts = append(parts, participantJSON(p))
	}
	return map[string]any{
		"jid":         info.JID.String(),
		"name":        info.Name,
		"description": info.Topic,
		// description_id is WhatsApp's optimistic-concurrency token for the
		// description (sent back as prev on the next change). Exposed because a
		// bad value here is what makes a description impossible to edit.
		"description_id": info.TopicID,
		"announce":       info.IsAnnounce,
		"locked":         info.IsLocked,
		"member_add":     memberAddModeString(info.MemberAddMode),
		"join_approval":  info.IsJoinApprovalRequired,
		// Community fields: is_community marks a community (parent) itself,
		// community_jid is the community a group belongs to, and
		// is_announcement_group marks that community's announcement group.
		"is_community":          info.IsParent,
		"community_jid":         optionalJID(info.LinkedParentJID),
		"is_announcement_group": info.IsDefaultSubGroup,
		"participants":          parts,
	}
}

func optionalJID(j types.JID) any {
	if j.IsEmpty() {
		return nil
	}
	return j.String()
}

// applySettings applies whichever optional settings are non-nil. It returns a
// map of setting name -> error rather than failing the whole call, because by
// the time these run the group already exists (or already had some settings
// changed) and the caller needs to know exactly which ones did not stick.
// member_add is expected to be pre-validated by the handler.
func applySettings(ctx context.Context, client *whatsmeow.Client, jid types.JID,
	name, desc *string, announce, locked *bool, memberAdd *string, joinApproval *bool) map[string]string {
	warn := map[string]string{}
	if name != nil {
		if err := client.SetGroupName(ctx, jid, *name); err != nil {
			warn["name"] = err.Error()
		}
	}
	if desc != nil {
		// SetGroupTopic, not SetGroupDescription. WhatsApp's own clients send
		// <description id="<new random id>" prev="<id being replaced>"> and
		// <description delete="true" …/> to clear. SetGroupDescription sends a
		// bare <description><body/></description>: the server accepts that
		// once and stores the id as the literal string "undefined", after which
		// every later change from ANY client (the phone included) is answered
		// with 409 conflict, and an empty body without delete is never answered
		// at all. SetGroupTopic fetches the current id for prev and generates
		// the new one; an empty description becomes a delete.
		if err := client.SetGroupTopic(ctx, jid, "", "", *desc); err != nil {
			warn["description"] = err.Error()
		}
	}
	if announce != nil {
		if err := client.SetGroupAnnounce(ctx, jid, *announce); err != nil {
			warn["announce"] = err.Error()
		}
	}
	if locked != nil {
		if err := client.SetGroupLocked(ctx, jid, *locked); err != nil {
			warn["locked"] = err.Error()
		}
	}
	if memberAdd != nil {
		mode, err := memberAddMode(*memberAdd)
		if err == nil {
			err = client.SetGroupMemberAddMode(ctx, jid, mode)
		}
		if err != nil {
			warn["member_add"] = err.Error()
		}
	}
	if joinApproval != nil {
		if err := client.SetGroupJoinApprovalMode(ctx, jid, *joinApproval); err != nil {
			warn["join_approval"] = err.Error()
		}
	}
	return warn
}

// fetchGroupInfo reads the group's current state on its own short budget, so a
// setting that used up the request's context still yields a response that
// shows what the group looks like now.
func fetchGroupInfo(parent context.Context, client *whatsmeow.Client, jid types.JID) (*types.GroupInfo, error) {
	ctx, cancel := context.WithTimeout(parent, groupInfoTimeout)
	defer cancel()
	return client.GetGroupInfo(ctx, jid)
}

func registerGroupRoutes(mux *http.ServeMux, client *whatsmeow.Client, auth func(http.HandlerFunc) http.HandlerFunc) {
	// POST /api/group/create
	mux.HandleFunc("/api/group/create", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var req groupCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if err := validateGroupName(req.Name); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		jids, err := parseUserJIDs(req.Participants)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// Validate member_add before creating anything: a typo here should be
		// a 400, not a group that exists with a warning attached.
		if req.MemberAdd != nil {
			if _, err := memberAddMode(*req.MemberAdd); err != nil {
				jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if !requireClient(w, client) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), groupRequestTimeout)
		defer cancel()

		// Everything CreateGroup can carry goes in the create IQ itself, so
		// the group is born with the right settings instead of flipping them
		// a moment later.
		cr := whatsmeow.ReqCreateGroup{Name: req.Name, Participants: jids}
		if req.Announce != nil {
			cr.GroupAnnounce = types.GroupAnnounce{IsAnnounce: *req.Announce}
		}
		if req.Locked != nil {
			cr.GroupLocked = types.GroupLocked{IsLocked: *req.Locked}
		}
		if req.JoinApproval != nil {
			cr.GroupMembershipApprovalMode = types.GroupMembershipApprovalMode{IsJoinApprovalRequired: *req.JoinApproval}
		}
		if req.Ephemeral != nil && *req.Ephemeral > 0 {
			cr.GroupEphemeral = types.GroupEphemeral{IsEphemeral: true, DisappearingTimer: *req.Ephemeral}
		}
		info, err := client.CreateGroup(ctx, cr)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "create failed: "+err.Error())
			return
		}
		// The create response is the only place per-participant refusals
		// (privacy settings etc.) are reported; GetGroupInfo below drops them.
		failed := failedParticipants(info.Participants)

		var desc *string
		if strings.TrimSpace(req.Description) != "" {
			desc = &req.Description
		}
		warn := map[string]string{}
		if desc != nil || req.MemberAdd != nil {
			sleepCtx(ctx, postCreateSettleDelay)
			warn = applySettings(ctx, client, info.JID, nil, desc, nil, nil, req.MemberAdd, nil)
			// Re-fetch so the response reflects the applied settings.
			if fresh, ferr := fetchGroupInfo(r.Context(), client, info.JID); ferr == nil {
				info = fresh
			} else {
				warn["refresh"] = "group created but re-fetching its info failed: " + ferr.Error()
			}
		}
		resp := map[string]any{"success": true, "group": groupInfoJSON(info)}
		if len(warn) > 0 {
			resp["warnings"] = warn
		}
		if len(failed) > 0 {
			resp["failed_participants"] = failed
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	// POST /api/group/settings
	mux.HandleFunc("/api/group/settings", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var req groupSettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(req.GroupJID) == "" {
			jsonErr(w, http.StatusBadRequest, "group_jid is required")
			return
		}
		jid, err := types.ParseJID(strings.TrimSpace(req.GroupJID))
		if err != nil || jid.Server != types.GroupServer {
			jsonErr(w, http.StatusBadRequest, "group_jid must be a @g.us JID")
			return
		}
		if req.Name != nil {
			trimmed := strings.TrimSpace(*req.Name)
			if err := validateGroupName(trimmed); err != nil {
				jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			req.Name = &trimmed
		}
		if req.MemberAdd != nil {
			if _, err := memberAddMode(*req.MemberAdd); err != nil {
				jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if !requireClient(w, client) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), groupRequestTimeout)
		defer cancel()
		// With no settings supplied this is simply a read of the current state.
		warn := applySettings(ctx, client, jid, req.Name, req.Description, req.Announce, req.Locked, req.MemberAdd, req.JoinApproval)
		info, err := fetchGroupInfo(r.Context(), client, jid)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "settings applied but info fetch failed: "+err.Error())
			return
		}
		resp := map[string]any{"success": len(warn) == 0, "group": groupInfoJSON(info)}
		if len(warn) > 0 {
			resp["warnings"] = warn
			resp["message"] = "some settings could not be applied; see warnings"
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	// POST /api/group/participants
	mux.HandleFunc("/api/group/participants", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var req groupParticipantsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(req.GroupJID) == "" {
			jsonErr(w, http.StatusBadRequest, "group_jid is required")
			return
		}
		jid, err := types.ParseJID(strings.TrimSpace(req.GroupJID))
		if err != nil || jid.Server != types.GroupServer {
			jsonErr(w, http.StatusBadRequest, "group_jid must be a @g.us JID")
			return
		}
		var action whatsmeow.ParticipantChange
		switch strings.ToLower(strings.TrimSpace(req.Action)) {
		case "add":
			action = whatsmeow.ParticipantChangeAdd
		case "remove":
			action = whatsmeow.ParticipantChangeRemove
		case "promote":
			action = whatsmeow.ParticipantChangePromote
		case "demote":
			action = whatsmeow.ParticipantChangeDemote
		default:
			jsonErr(w, http.StatusBadRequest, "action must be add, remove, promote or demote")
			return
		}
		jids, err := parseUserJIDs(req.Participants)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if !requireClient(w, client) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), groupRequestTimeout)
		defer cancel()
		results, err := client.UpdateGroupParticipants(ctx, jid, jids, action)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		out := make([]map[string]any, 0, len(results))
		allOK := true
		for _, res := range results {
			entry := participantJSON(res)
			entry["ok"] = res.Error == 0
			if res.Error != 0 {
				allOK = false
			}
			out = append(out, entry)
		}
		resp := map[string]any{"success": allOK, "action": string(action), "results": out}
		if !allOK {
			resp["message"] = "some participants could not be updated; see results"
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	// GET /api/profile-picture?jid=...&preview=1
	//
	// Works for contacts (phone, @s.whatsapp.net, @lid) and groups (@g.us).
	// "No picture" and "hidden from you" are normal outcomes for an avatar
	// lookup, so they return 200 with url: null and a reason rather than 502.
	mux.HandleFunc("/api/profile-picture", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			jsonErr(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		raw := r.URL.Query().Get("jid")
		if strings.TrimSpace(raw) == "" {
			jsonErr(w, http.StatusBadRequest, "jid is required")
			return
		}
		jid, err := parseJIDOrPhone(raw)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if !requireClient(w, client) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), pictureRequestTimeout)
		defer cancel()
		preview := r.URL.Query().Get("preview")
		params := &whatsmeow.GetProfilePictureParams{Preview: preview == "1" || strings.EqualFold(preview, "true")}
		pic, err := client.GetProfilePictureInfo(ctx, jid, params)
		resp := map[string]any{"success": true, "jid": jid.String(), "url": nil}
		switch {
		case errors.Is(err, whatsmeow.ErrProfilePictureNotSet):
			resp["reason"] = "not_set"
		case errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized):
			resp["reason"] = "hidden"
		case err != nil:
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		case pic == nil:
			resp["reason"] = "unchanged"
		default:
			resp["url"] = pic.URL
			resp["id"] = pic.ID
			resp["type"] = pic.Type
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	registerCommunityRoutes(mux, client, auth)
}
