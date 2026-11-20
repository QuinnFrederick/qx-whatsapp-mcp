package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	server := map[types.JID][]*types.GroupLinkTarget{
		forest: {
			{JID: gjid("103"), GroupName: types.GroupName{Name: "Archery"}},
			{JID: gjid("102"), GroupName: types.GroupName{Name: "camping"}},
			{JID: gjid("101"), GroupName: types.GroupName{Name: "Forest Announcements"}, GroupIsDefaultSub: types.GroupIsDefaultSub{IsDefaultSubGroup: true}},
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
		{"list with no client is 503", http.MethodGet, "/api/communities", "", http.StatusServiceUnavailable},
		{"list without sub-groups with no client is 503", http.MethodGet, "/api/communities?sub_groups=0", "", http.StatusServiceUnavailable},

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
