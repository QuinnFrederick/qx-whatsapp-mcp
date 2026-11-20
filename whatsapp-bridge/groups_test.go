package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestParseJIDOrPhone(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"bare digits become a user JID", "12025551234", "12025551234@s.whatsapp.net", false},
		{"plus and punctuation are stripped", "+1 (202) 555-1234", "12025551234@s.whatsapp.net", false},
		{"user JID passes through", "12025551234@s.whatsapp.net", "12025551234@s.whatsapp.net", false},
		{"device suffix is dropped", "12025551234:12@s.whatsapp.net", "12025551234@s.whatsapp.net", false},
		{"lid JID passes through", "184125298348272@lid", "184125298348272@lid", false},
		{"group JID passes through", "120363012345678901@g.us", "120363012345678901@g.us", false},
		{"blank is rejected", "   ", "", true},
		{"letters only is rejected", "harry", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJIDOrPhone(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseJIDOrPhone(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJIDOrPhone(%q) error: %v", tc.in, err)
			}
			if got.String() != tc.want {
				t.Fatalf("parseJIDOrPhone(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseUserJIDs(t *testing.T) {
	t.Run("dedupes and skips blanks", func(t *testing.T) {
		got, err := parseUserJIDs([]string{"12025551234", " ", "+1 202 555 1234", "184125298348272@lid"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"12025551234@s.whatsapp.net", "184125298348272@lid"}
		if len(got) != len(want) {
			t.Fatalf("got %d JIDs %v, want %d", len(got), got, len(want))
		}
		for i := range want {
			if got[i].String() != want[i] {
				t.Fatalf("jid[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
	t.Run("rejects group JIDs", func(t *testing.T) {
		if _, err := parseUserJIDs([]string{"120363012345678901@g.us"}); err == nil {
			t.Fatal("expected error for group JID participant")
		}
	})
	t.Run("rejects empty list", func(t *testing.T) {
		if _, err := parseUserJIDs(nil); err == nil {
			t.Fatal("expected error for empty participants")
		}
		if _, err := parseUserJIDs([]string{"", "  "}); err == nil {
			t.Fatal("expected error for blank-only participants")
		}
	})
}

func TestMemberAddModeRoundTrip(t *testing.T) {
	cases := map[string]types.GroupMemberAddMode{
		"all_members": types.GroupMemberAddModeAllMember,
		"ALL":         types.GroupMemberAddModeAllMember,
		"everyone":    types.GroupMemberAddModeAllMember,
		"admins_only": types.GroupMemberAddModeAdmin,
		" admins ":    types.GroupMemberAddModeAdmin,
	}
	for in, want := range cases {
		got, err := memberAddMode(in)
		if err != nil || got != want {
			t.Fatalf("memberAddMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := memberAddMode("friends"); err == nil {
		t.Fatal("expected error for unknown member_add value")
	}
	if got := memberAddModeString(types.GroupMemberAddModeAdmin); got != "admins_only" {
		t.Fatalf("memberAddModeString(admin) = %q", got)
	}
	if got := memberAddModeString(types.GroupMemberAddModeAllMember); got != "all_members" {
		t.Fatalf("memberAddModeString(all) = %q", got)
	}
}

func TestParticipantJSONSurfacesErrors(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	p := types.GroupParticipant{
		JID:         types.NewJID("12025551234", types.DefaultUserServer),
		PhoneNumber: types.NewJID("12025551234", types.DefaultUserServer),
		Error:       403,
		AddRequest:  &types.GroupParticipantAddRequest{Code: "ABC123", Expiration: exp},
	}
	got := participantJSON(p)
	if got["error"] != 403 {
		t.Fatalf("error = %v, want 403", got["error"])
	}
	if hint, _ := got["error_hint"].(string); !strings.Contains(hint, "privacy") {
		t.Fatalf("error_hint = %q, want privacy explanation", hint)
	}
	ar, ok := got["add_request"].(map[string]any)
	if !ok || ar["code"] != "ABC123" || ar["expiration"] != exp.Unix() {
		t.Fatalf("add_request = %v", got["add_request"])
	}
	if got["phone"] != "12025551234" || got["lid"] != nil {
		t.Fatalf("phone/lid = %v/%v", got["phone"], got["lid"])
	}

	ok2 := participantJSON(types.GroupParticipant{JID: p.JID})
	if _, has := ok2["error"]; has {
		t.Fatal("successful participant should not carry an error key")
	}
	if failed := failedParticipants([]types.GroupParticipant{p, {JID: p.JID}}); len(failed) != 1 {
		t.Fatalf("failedParticipants returned %d entries, want 1", len(failed))
	}
}

func TestGroupInfoJSONExposesDescriptionID(t *testing.T) {
	info := &types.GroupInfo{
		JID:       types.NewJID("120363012345678901", types.GroupServer),
		GroupName: types.GroupName{Name: "Trip"},
		GroupTopic: types.GroupTopic{
			Topic:   "Plans",
			TopicID: "3EB0ABCDEF0123456789",
		},
	}
	got := groupInfoJSON(info)
	if got["description"] != "Plans" || got["description_id"] != "3EB0ABCDEF0123456789" {
		t.Fatalf("description/description_id = %v/%v", got["description"], got["description_id"])
	}
	if parts, ok := got["participants"].([]map[string]any); !ok || len(parts) != 0 {
		t.Fatalf("participants = %v, want empty slice", got["participants"])
	}
}

func TestValidateGroupNameCountsCharactersUpTo100(t *testing.T) {
	if maxGroupNameRunes != 100 {
		t.Fatalf("maxGroupNameRunes = %d, want 100 (WhatsApp's subject limit)", maxGroupNameRunes)
	}
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", true},
		{"26 chars, over the old 25 cap", strings.Repeat("x", 26), false},
		{"100 multibyte chars (200 bytes)", strings.Repeat("é", 100), false},
		{"101 chars", strings.Repeat("x", 101), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateGroupName(tc.in); (err != nil) != tc.wantErr {
				t.Fatalf("validateGroupName(%d runes) err = %v, wantErr %v", len([]rune(tc.in)), err, tc.wantErr)
			}
		})
	}
}

// Names must reach WhatsApp and come back byte-for-byte. Emoji such as
// heart-on-fire are several code points glued together by a zero-width joiner
// (U+200D) and a variation selector (U+FE0F); neither is Unicode White_Space, so
// strings.TrimSpace keeps them even at the very end of a name, and JSON encoding
// in both directions preserves them.
func TestGroupNamePreservesEmojiSequences(t *testing.T) {
	heartOnFire := "\u2764\ufe0f\u200d\U0001f525" // red heart + VS16 + ZWJ + fire
	names := []string{
		"Commons \U0001f333" + heartOnFire + " Forest of Wild Love",
		"Ends in a ZWJ sequence " + heartOnFire,
		"Ends in a variation selector \u2764\ufe0f",
		"Family \U0001f468\u200d\U0001f469\u200d\U0001f467",
	}
	for _, name := range names {
		if got := strings.TrimSpace(" " + name + "\n"); got != name {
			t.Errorf("TrimSpace changed %+q to %+q", name, got)
		}
		if err := validateGroupName(name); err != nil {
			t.Errorf("validateGroupName(%+q) = %v", name, err)
		}
		// Request side: an ASCII-escaped JSON body decodes to the exact runes.
		escaped, _ := json.Marshal(map[string]string{"name": name})
		escaped = []byte(strings.NewReplacer("\u200d", `\u200d`, "\ufe0f", `\ufe0f`).Replace(string(escaped)))
		var req groupSettingsRequest
		if err := json.Unmarshal(escaped, &req); err != nil || req.Name == nil || *req.Name != name {
			t.Errorf("decoding %s gave %v (err %v), want %+q", escaped, req.Name, err, name)
		}
		if strings.Contains(name, "\u200d") && !strings.Contains(string(escaped), `\u200d`) {
			t.Errorf("test setup: %s should carry an escaped ZWJ", escaped)
		}
		// Response side: the group object round-trips through JSON unchanged.
		out, err := json.Marshal(groupInfoJSON(&types.GroupInfo{GroupName: types.GroupName{Name: name}}))
		if err != nil {
			t.Fatal(err)
		}
		var back map[string]any
		if err := json.Unmarshal(out, &back); err != nil || back["name"] != name {
			t.Errorf("response name = %+q, want %+q (err %v)", back["name"], name, err)
		}
	}
}

// The real auth wrapper is covered by auth_test.go; an identity wrapper keeps
// these focused on request validation. A nil client is deliberate: every
// validation failure must be reported before the client is touched, and once
// input is valid a missing client must read as 503, not a panic.
func TestGroupEndpointValidation(t *testing.T) {
	identity := func(h http.HandlerFunc) http.HandlerFunc { return h }
	mux := http.NewServeMux()
	registerGroupRoutes(mux, nil, identity)

	longName := strings.Repeat("x", maxGroupNameRunes+1)
	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"create rejects GET", http.MethodGet, "/api/group/create", "", http.StatusMethodNotAllowed},
		{"create rejects invalid JSON", http.MethodPost, "/api/group/create", "{", http.StatusBadRequest},
		{"create requires name", http.MethodPost, "/api/group/create", `{"participants":["12025551234"]}`, http.StatusBadRequest},
		{"create rejects long name", http.MethodPost, "/api/group/create", `{"name":"` + longName + `","participants":["12025551234"]}`, http.StatusBadRequest},
		{"create requires participants", http.MethodPost, "/api/group/create", `{"name":"Trip"}`, http.StatusBadRequest},
		{"create rejects bad member_add before creating", http.MethodPost, "/api/group/create", `{"name":"Trip","participants":["12025551234"],"member_add":"friends"}`, http.StatusBadRequest},
		{"create with no client is 503", http.MethodPost, "/api/group/create", `{"name":"Trip","participants":["12025551234"]}`, http.StatusServiceUnavailable},

		{"settings rejects GET", http.MethodGet, "/api/group/settings", "", http.StatusMethodNotAllowed},
		{"settings requires group_jid", http.MethodPost, "/api/group/settings", `{"name":"x"}`, http.StatusBadRequest},
		{"settings rejects user JID", http.MethodPost, "/api/group/settings", `{"group_jid":"12025551234@s.whatsapp.net"}`, http.StatusBadRequest},
		{"settings rejects blank name", http.MethodPost, "/api/group/settings", `{"group_jid":"120363012345678901@g.us","name":"  "}`, http.StatusBadRequest},
		{"settings rejects bad member_add", http.MethodPost, "/api/group/settings", `{"group_jid":"120363012345678901@g.us","member_add":"nobody"}`, http.StatusBadRequest},
		{"settings with no client is 503", http.MethodPost, "/api/group/settings", `{"group_jid":"120363012345678901@g.us"}`, http.StatusServiceUnavailable},

		{"participants requires group_jid", http.MethodPost, "/api/group/participants", `{"participants":["12025551234"],"action":"add"}`, http.StatusBadRequest},
		{"participants rejects bad action", http.MethodPost, "/api/group/participants", `{"group_jid":"120363012345678901@g.us","participants":["12025551234"],"action":"kick"}`, http.StatusBadRequest},
		{"participants rejects group as participant", http.MethodPost, "/api/group/participants", `{"group_jid":"120363012345678901@g.us","participants":["120363012345678902@g.us"],"action":"add"}`, http.StatusBadRequest},
		{"participants with no client is 503", http.MethodPost, "/api/group/participants", `{"group_jid":"120363012345678901@g.us","participants":["12025551234"],"action":"Remove"}`, http.StatusServiceUnavailable},

		{"profile picture rejects POST", http.MethodPost, "/api/profile-picture", "", http.StatusMethodNotAllowed},
		{"profile picture requires jid", http.MethodGet, "/api/profile-picture", "", http.StatusBadRequest},
		{"profile picture rejects garbage jid", http.MethodGet, "/api/profile-picture?jid=harry", "", http.StatusBadRequest},
		{"profile picture with no client is 503", http.MethodGet, "/api/profile-picture?jid=12025551234&preview=1", "", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			if !strings.Contains(rec.Body.String(), `"success":false`) {
				t.Fatalf("body should be a JSON error envelope, got %s", rec.Body.String())
			}
		})
	}
}
