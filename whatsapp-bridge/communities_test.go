package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func gjid(id string) types.JID { return types.NewJID(id, types.GroupServer) }

func TestBuildCommunities(t *testing.T) {
	me := types.NewJID("15550000001", types.DefaultUserServer)
	meLID := types.NewJID("99000001", types.HiddenUserServer)
	forest, book := gjid("100"), gjid("200")
	joined := []*types.GroupInfo{
		{
			JID:          forest,
			GroupName:    types.GroupName{Name: "Forest"},
			GroupTopic:   types.GroupTopic{Topic: "Woods"},
			GroupParent:  types.GroupParent{IsParent: true},
			Participants: []types.GroupParticipant{{JID: meLID, IsAdmin: true}},
		},
		{JID: gjid("101"), GroupName: types.GroupName{Name: "Forest Announcements"}, GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: forest}, GroupIsDefaultSub: types.GroupIsDefaultSub{IsDefaultSubGroup: true}},
		{JID: gjid("102"), GroupName: types.GroupName{Name: "camping"}, GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: forest}},
		// Book club: we are in a linked group but the parent was not listed,
		// and its full sub-group list is unavailable.
		{JID: gjid("201"), GroupName: types.GroupName{Name: "Readers"}, GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: book}},
		// Plain groups are not communities.
		{JID: gjid("300"), GroupName: types.GroupName{Name: "Family"}},
		nil,
	}
	server := map[types.JID][]communitySubGroup{
		forest: {
			{JID: gjid("103"), Name: "Archery", Size: 12, IsHidden: true},
			{JID: gjid("102"), Name: "camping", IsGeneralChat: true},
			{JID: gjid("101"), Name: "Forest Announcements", Size: 40, IsAnnouncement: true},
		},
	}

	got := buildCommunities(joined, server, []types.JID{me, meLID})
	if len(got) != 2 {
		t.Fatalf("got %d communities, want 2: %v", len(got), got)
	}
	// Sorted by name: "" (unknown parent) before "Forest".
	unknown, f := got[0], got[1]

	if f["jid"] != forest.String() || f["name"] != "Forest" || f["description"] != "Woods" {
		t.Errorf("forest header = %v", f)
	}
	if f["you_are_admin"] != true || f["sub_groups_source"] != "server" {
		t.Errorf("forest you_are_admin/source = %v/%v", f["you_are_admin"], f["sub_groups_source"])
	}
	ann, _ := f["announcement_group"].(map[string]any)
	if ann == nil || ann["jid"] != gjid("101").String() || ann["joined"] != true {
		t.Errorf("forest announcement_group = %v", f["announcement_group"])
	}
	subs := f["sub_groups"].([]map[string]any)
	var order []string
	for _, s := range subs {
		order = append(order, s["name"].(string))
	}
	if strings.Join(order, ",") != "Forest Announcements,Archery,camping" {
		t.Errorf("forest sub_group order = %v (announcement first, then by name)", order)
	}
	if subs[1]["joined"] != false || subs[2]["joined"] != true {
		t.Errorf("joined flags = %v / %v; Archery is not joined, camping is", subs[1]["joined"], subs[2]["joined"])
	}
	if subs[1]["hidden"] != true || subs[1]["size"] != 12 || subs[2]["is_general_chat"] != true || subs[2]["size"] != nil {
		t.Errorf("server flags: archery %v, camping %v", subs[1], subs[2])
	}
	bookSub := unknownSubs(t, got[0])
	if bookSub["hidden"] != nil || bookSub["is_general_chat"] != nil {
		t.Errorf("joined_only entries must report hidden/general chat as unknown: %v", bookSub)
	}

	if unknown["jid"] != book.String() || unknown["sub_groups_source"] != "joined_only" || unknown["you_are_admin"] != nil {
		t.Errorf("book club = %v", unknown)
	}
	if unknown["announcement_group"] != nil || len(unknown["sub_groups"].([]map[string]any)) != 1 {
		t.Errorf("book club subs = %v, announcement = %v", unknown["sub_groups"], unknown["announcement_group"])
	}
}

func TestAdminStatus(t *testing.T) {
	me := types.NewJID("15550000001", types.DefaultUserServer)
	info := &types.GroupInfo{Participants: []types.GroupParticipant{
		{JID: types.NewJID("15550000002", types.DefaultUserServer), IsAdmin: true},
		{JID: types.NewJID("99000001", types.HiddenUserServer), PhoneNumber: me},
	}}
	if got := adminStatus(info, []types.JID{me}); got != false {
		t.Errorf("member matched by phone: got %v, want false", got)
	}
	info.Participants[1].IsSuperAdmin = true
	if got := adminStatus(info, []types.JID{me}); got != true {
		t.Errorf("super admin: got %v, want true", got)
	}
	if got := adminStatus(info, []types.JID{types.NewJID("1", types.DefaultUserServer)}); got != nil {
		t.Errorf("absent: got %v, want nil", got)
	}
}

func TestGroupInfoJSONCommunityFields(t *testing.T) {
	sub := groupInfoJSON(&types.GroupInfo{
		JID:               gjid("101"),
		GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: gjid("100")},
		GroupIsDefaultSub: types.GroupIsDefaultSub{IsDefaultSubGroup: true},
	})
	if sub["community_jid"] != "100@g.us" || sub["is_announcement_group"] != true || sub["is_community"] != false {
		t.Errorf("sub-group fields = %v", sub)
	}
	plain := groupInfoJSON(&types.GroupInfo{JID: gjid("300")})
	if plain["community_jid"] != nil || plain["is_announcement_group"] != false {
		t.Errorf("plain group fields = %v", plain)
	}
	parent := groupInfoJSON(&types.GroupInfo{JID: gjid("100"), GroupParent: types.GroupParent{IsParent: true}})
	if parent["is_community"] != true {
		t.Errorf("community fields = %v", parent)
	}
}

// Same approach as TestGroupEndpointValidation: identity auth, nil client.
func TestCommunityEndpointValidation(t *testing.T) {
	identity := func(h http.HandlerFunc) http.HandlerFunc { return h }
	mux := http.NewServeMux()
	registerGroupRoutes(mux, nil, identity)

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"list rejects POST", http.MethodPost, "/api/communities", "{}", http.StatusMethodNotAllowed},
		{"participants rejects POST", http.MethodPost, "/api/community/participants?community_jid=1@g.us", "{}", http.StatusMethodNotAllowed},
		{"participants requires community", http.MethodGet, "/api/community/participants", "", http.StatusBadRequest},
		{"participants rejects user JID", http.MethodGet, "/api/community/participants?community_jid=1@s.whatsapp.net", "", http.StatusBadRequest},
		{"participants with no client is 503", http.MethodGet, "/api/community/participants?community_jid=1@g.us", "", http.StatusServiceUnavailable},
		{"create rejects GET", http.MethodGet, "/api/community/create", "", http.StatusMethodNotAllowed},
		{"create rejects invalid JSON", http.MethodPost, "/api/community/create", "{", http.StatusBadRequest},
		{"create requires name", http.MethodPost, "/api/community/create", `{"name":"  ","description":"x"}`, http.StatusBadRequest},
		{"create rejects long name", http.MethodPost, "/api/community/create", `{"name":"` + strings.Repeat("x", maxGroupNameRunes+1) + `"}`, http.StatusBadRequest},
		{"create with no client is 503", http.MethodPost, "/api/community/create", `{"name":"Forest"}`, http.StatusServiceUnavailable},
		{"list with no client is 503", http.MethodGet, "/api/communities", "", http.StatusServiceUnavailable},
		{"list without sub-groups with no client is 503", http.MethodGet, "/api/communities?sub_groups=0", "", http.StatusServiceUnavailable},
		{"list rejects user JID filter", http.MethodGet, "/api/communities?community_jid=1@s.whatsapp.net", "", http.StatusBadRequest},
		{"list one community with no client is 503", http.MethodGet, "/api/communities?community_jid=1@g.us", "", http.StatusServiceUnavailable},

		{"groups rejects GET", http.MethodGet, "/api/community/groups", "", http.StatusMethodNotAllowed},
		{"groups rejects invalid JSON", http.MethodPost, "/api/community/groups", "{", http.StatusBadRequest},
		{"groups requires community", http.MethodPost, "/api/community/groups", `{"group_jids":["1@g.us"],"action":"link"}`, http.StatusBadRequest},
		{"groups rejects user JID community", http.MethodPost, "/api/community/groups", `{"community_jid":"1@s.whatsapp.net","group_jids":["2@g.us"],"action":"link"}`, http.StatusBadRequest},
		{"groups rejects bad action", http.MethodPost, "/api/community/groups", `{"community_jid":"1@g.us","group_jids":["2@g.us"],"action":"move"}`, http.StatusBadRequest},
		{"groups requires a group", http.MethodPost, "/api/community/groups", `{"community_jid":"1@g.us","group_jids":[" "],"action":"link"}`, http.StatusBadRequest},
		{"groups rejects user JID group", http.MethodPost, "/api/community/groups", `{"community_jid":"1@g.us","group_jids":["2@s.whatsapp.net"],"action":"unlink"}`, http.StatusBadRequest},
		{"groups rejects linking community to itself", http.MethodPost, "/api/community/groups", `{"community_jid":"1@g.us","group_jids":["1@g.us"],"action":"link"}`, http.StatusBadRequest},
		{"link with no client is 503", http.MethodPost, "/api/community/groups", `{"community_jid":"1@g.us","group_jids":["2@g.us"],"action":"LINK"}`, http.StatusServiceUnavailable},
		{"unlink with no client is 503", http.MethodPost, "/api/community/groups", `{"community_jid":"1@g.us","group_jids":["2@g.us"],"action":"unlink"}`, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestQueryBool(t *testing.T) {
	for raw, want := range map[string]bool{"": true, "1": true, "true": true, "0": false, "False": false, "no": false, "junk": true} {
		r := httptest.NewRequest(http.MethodGet, "/x?sub_groups="+raw, nil)
		if got := queryBool(r, "sub_groups", true); got != want {
			t.Errorf("queryBool(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseGroupJIDs(t *testing.T) {
	got, err := parseGroupJIDs([]string{"2@g.us", " ", "3@g.us", "2@g.us "}, "group_jids")
	if err != nil || len(got) != 2 || got[0] != gjid("2") || got[1] != gjid("3") {
		t.Fatalf("parseGroupJIDs = %v, %v; want [2@g.us 3@g.us] (blanks and duplicates dropped)", got, err)
	}
	if _, err := parseGroupJIDs([]string{"2@g.us", "15550000001"}, "group_jids"); err == nil || !strings.Contains(err.Error(), `"15550000001"`) {
		t.Errorf("bad entry err = %v, want it to name the entry", err)
	}
	if _, err := parseGroupJIDs(nil, "group_jids"); err == nil {
		t.Error("empty list should fail")
	}
}

func TestNewCommunityRequest(t *testing.T) {
	req := newCommunityRequest("Forest")
	if req.Name != "Forest" || !req.IsParent {
		t.Fatalf("request = %+v, want a parent named Forest", req)
	}
	if len(req.Participants) != 0 || !req.LinkedParentJID.IsEmpty() {
		t.Errorf("a community is created without participants or a parent: %+v", req)
	}
}

type fakeLIDs map[types.JID]types.JID

func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	if pn, ok := f[lid]; ok {
		return pn, nil
	}
	return types.JID{}, errors.New("unknown")
}

func (f fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	for lid, p := range f {
		if p == pn {
			return lid, nil
		}
	}
	return types.JID{}, nil // whatsmeow returns an empty JID when unmapped
}

func TestCommunityMembersJSON(t *testing.T) {
	pn := types.NewJID("15550000001", types.DefaultUserServer)
	lid := types.NewJID("99000001", types.HiddenUserServer)
	strangerLID := types.NewJID("99000002", types.HiddenUserServer)
	device := types.JID{User: "15550000003", Device: 4, Server: types.DefaultUserServer}
	lids := fakeLIDs{lid: pn}

	got := communityMembersJSON(context.Background(), []types.JID{lid, pn, strangerLID, device}, lids)
	want := []map[string]any{
		{"jid": "99000001@lid", "phone": "15550000001", "lid": "99000001"},
		{"jid": "15550000001@s.whatsapp.net", "phone": "15550000001", "lid": "99000001"},
		{"jid": "99000002@lid", "phone": nil, "lid": "99000002"},
		{"jid": "15550000003@s.whatsapp.net", "phone": "15550000003", "lid": nil},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d", len(got), len(want))
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("member %d %s = %v, want %v", i, k, got[i][k], v)
			}
		}
	}
	if none := communityMembersJSON(context.Background(), []types.JID{lid}, nil); none[0]["phone"] != nil {
		t.Errorf("nil resolver should leave phone null, got %v", none[0])
	}
}

func TestFilterCommunities(t *testing.T) {
	all := []map[string]any{{"jid": "1@g.us"}, {"jid": "2@g.us"}}
	if got := filterCommunities(all, types.JID{}); len(got) != 2 {
		t.Errorf("no filter kept %d, want 2", len(got))
	}
	if got := filterCommunities(all, gjid("2")); len(got) != 1 || got[0]["jid"] != "2@g.us" {
		t.Errorf("filter 2 = %v", got)
	}
	if got := filterCommunities(all, gjid("3")); got == nil || len(got) != 0 {
		t.Errorf("unknown filter = %#v, want empty non-nil slice", got)
	}
}

func TestCollectSubGroupsStopsAfterRateLimit(t *testing.T) {
	var asked []string
	fetch := func(jid types.JID) (int, error) {
		asked = append(asked, jid.User)
		switch jid.User {
		case "2":
			return 0, errors.New("item-not-found")
		case "3":
			return 0, fmt.Errorf("info query returned status 429: %w", whatsmeow.ErrIQRateOverLimit)
		}
		return 7, nil
	}
	got, warn := collectSubGroups([]types.JID{gjid("1"), gjid("2"), gjid("3"), gjid("4"), gjid("5")}, fetch)

	if strings.Join(asked, ",") != "1,2,3" {
		t.Errorf("queried %v; a plain error must not stop the loop, a 429 must", asked)
	}
	if len(got) != 1 || got[gjid("1")] != 7 {
		t.Errorf("results = %v, want only community 1", got)
	}
	for _, id := range []string{"2", "3"} {
		if !strings.Contains(warn[id+"@g.us"], "unavailable") {
			t.Errorf("warning for %s = %q", id, warn[id+"@g.us"])
		}
	}
	for _, id := range []string{"4", "5"} {
		if !strings.Contains(warn[id+"@g.us"], "rate-limiting") {
			t.Errorf("warning for %s = %q, want a skipped-for-rate-limit note", id, warn[id+"@g.us"])
		}
	}
}

func unknownSubs(t *testing.T, c map[string]any) map[string]any {
	t.Helper()
	subs := c["sub_groups"].([]map[string]any)
	if len(subs) == 0 {
		t.Fatalf("community %v has no sub_groups", c["jid"])
	}
	return subs[0]
}

// Shape of a live response (values made up): attributes id/subject/s_t/size
// and child flags default_sub_group, general_chat and hidden_group.
func TestParseSubGroupsNode(t *testing.T) {
	group := func(id, subject string, attrs waBinary.Attrs, flags ...string) waBinary.Node {
		a := waBinary.Attrs{"id": id, "subject": subject, "s_t": "1700000000"}
		for k, v := range attrs {
			a[k] = v
		}
		var children []waBinary.Node
		for _, f := range flags {
			children = append(children, waBinary.Node{Tag: f})
		}
		return waBinary.Node{Tag: "group", Attrs: a, Content: children}
	}
	res := &waBinary.Node{Tag: "iq", Content: []waBinary.Node{{Tag: "sub_groups", Content: []waBinary.Node{
		group("101", "Announcements", waBinary.Attrs{"size": "40"}, "default_sub_group"),
		group("102", "General", waBinary.Attrs{"size": "35"}, "general_chat"),
		group("103", "Moderators", waBinary.Attrs{"size": "3"}, "hidden_group"),
		group("104", "No size", nil),
		{Tag: "something_else"},
	}}}}

	got, err := parseSubGroupsNode(res)
	if err != nil {
		t.Fatal(err)
	}
	want := []communitySubGroup{
		{JID: gjid("101"), Name: "Announcements", Size: 40, IsAnnouncement: true},
		{JID: gjid("102"), Name: "General", Size: 35, IsGeneralChat: true},
		{JID: gjid("103"), Name: "Moderators", Size: 3, IsHidden: true},
		{JID: gjid("104"), Name: "No size"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if _, err := parseSubGroupsNode(&waBinary.Node{Tag: "iq"}); err == nil {
		t.Error("a response without sub_groups must be an error")
	}
	bad := &waBinary.Node{Tag: "iq", Content: []waBinary.Node{{Tag: "sub_groups", Content: []waBinary.Node{{Tag: "group"}}}}}
	if _, err := parseSubGroupsNode(bad); err == nil {
		t.Error("a group without id or jid must be an error")
	}
}
