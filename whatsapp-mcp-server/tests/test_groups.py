"""Tests for the group management, profile picture and resolve_contacts helpers (fork)."""

import json

import pytest

import main as mcp_main
import whatsapp


class DummyResponse:
    def __init__(self, status_code=200, payload=None, text="OK"):
        self.status_code = status_code
        self._payload = payload
        self.text = text

    def json(self):
        if self._payload is None:
            raise ValueError("not json")
        return self._payload


def _capture_post(monkeypatch, payload=None, status_code=200, text="OK"):
    calls = []

    def fake_post(url, json, headers=None):
        calls.append({"url": url, "json": json, "headers": headers})
        return DummyResponse(status_code=status_code, payload=payload, text=text)

    monkeypatch.setattr(whatsapp.requests, "post", fake_post)
    return calls


def _capture_get(monkeypatch, payload=None, status_code=200, text="OK"):
    calls = []

    def fake_get(url, params=None, headers=None, **kwargs):
        calls.append({"url": url, "params": params, "headers": headers, "kwargs": kwargs})
        return DummyResponse(status_code=status_code, payload=payload, text=text)

    monkeypatch.setattr(whatsapp.requests, "get", fake_get)
    return calls


# --- create_group ---------------------------------------------------------


def test_create_group_posts_full_payload(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")
    group = {"jid": "120363012345678901@g.us", "name": "Trip"}
    calls = _capture_post(monkeypatch, payload={"success": True, "group": group})

    result = whatsapp.create_group(
        "  Trip  ",
        ["12025551234", " ", "184125298348272@lid"],
        description="  Summer plans ",
        announce=True,
        locked=False,
        member_add="Admins Only",
        join_approval=True,
    )

    assert result["success"] is True
    assert result["group"] == group
    assert result["message"] == "OK"
    assert calls[0]["url"] == f"{whatsapp.WHATSAPP_API_BASE_URL}/group/create"
    assert calls[0]["headers"] == {"Authorization": "Bearer test-token"}
    assert calls[0]["json"] == {
        "name": "Trip",
        "participants": ["12025551234", "184125298348272@lid"],
        "description": "Summer plans",
        "announce": True,
        "locked": False,
        "member_add": "admins_only",
        "join_approval": True,
    }


def test_create_group_omits_unset_settings_and_accepts_single_participant_string(monkeypatch):
    calls = _capture_post(monkeypatch, payload={"success": True})

    whatsapp.create_group("Trip", "12025551234")

    assert calls[0]["json"] == {"name": "Trip", "participants": ["12025551234"]}


@pytest.mark.parametrize(
    ("name", "participants", "member_add", "expected"),
    [
        ("", ["12025551234"], None, "name"),
        ("x" * 101, ["12025551234"], None, "100 characters"),
        ("Trip", [], None, "participant"),
        ("Trip", ["  "], None, "participant"),
        ("Trip", ["12025551234"], "friends", "member_add"),
    ],
)
def test_create_group_validates_before_calling_bridge(monkeypatch, name, participants, member_add, expected):
    calls = _capture_post(monkeypatch, payload={"success": True})

    result = whatsapp.create_group(name, participants, member_add=member_add)

    assert result["success"] is False
    assert expected in result["message"]
    assert calls == []


def test_group_name_limit_is_100_characters_not_bytes(monkeypatch):
    calls = _capture_post(monkeypatch, payload={"success": True})
    # 100 characters but 200 UTF-8 bytes; 26 characters was rejected under the old 25 cap.
    names = ["é" * 100, "x" * 26]

    for name in names:
        whatsapp.create_group(name, ["12025551234"])
        whatsapp.update_group_settings("120363012345678901@g.us", name=name)

    assert whatsapp.MAX_GROUP_NAME_LENGTH == 100
    assert [c["json"]["name"] for c in calls] == [n for n in names for _ in range(2)]


# Heart-on-fire is red heart + VS16 (U+FE0F) + ZWJ (U+200D) + fire. Neither joiner is
# whitespace, so str.strip() must keep them, even at the end of a name.
HEART_ON_FIRE = "❤️‍\U0001f525"
EMOJI_NAMES = [
    "Commons \U0001f333" + HEART_ON_FIRE + " Forest of Wild Love",
    "Ends in a ZWJ sequence " + HEART_ON_FIRE,
    "Ends in a variation selector ❤️",
    "Family \U0001f468‍\U0001f469‍\U0001f467",
]


@pytest.mark.parametrize("name", EMOJI_NAMES)
def test_group_names_preserve_zwj_sequences_and_variation_selectors(monkeypatch, name):
    calls = _capture_post(monkeypatch, payload={"success": True, "group": {"name": name}})

    created = whatsapp.create_group(f"  {name}\n", ["12025551234"])
    whatsapp.update_group_settings("120363012345678901@g.us", name=f" {name} ")

    assert [c["json"]["name"] for c in calls] == [name, name]
    # requests serialises with json.dumps (ASCII escapes); decoding gives back every code point.
    assert json.loads(json.dumps(calls[0]["json"]))["name"] == name
    assert created["group"]["name"] == name


async def test_mcp_tool_output_keeps_emoji_sequences_intact(monkeypatch):
    name = EMOJI_NAMES[0]
    _capture_post(monkeypatch, payload={"success": True, "group": {"name": name}})

    result = await mcp_main.mcp.call_tool("update_group_settings", {"group_jid": "120363012345678901@g.us"})

    content = result[0] if isinstance(result, tuple) else result
    assert json.loads(content[0].text)["group"]["name"] == name
    assert name in content[0].text  # emitted as raw UTF-8, not split or escaped


def test_create_group_inside_community_sends_community_jid(monkeypatch):
    calls = _capture_post(monkeypatch, payload={"success": True})

    whatsapp.create_group("Archery", ["12025551234"], community_jid=" 120363000000000100@g.us ")
    whatsapp.create_group("Standalone", ["12025551234"], community_jid="  ")

    assert calls[0]["json"]["community_jid"] == "120363000000000100@g.us"
    assert "community_jid" not in calls[1]["json"]


def test_create_group_rejects_non_group_community_jid(monkeypatch):
    calls = _capture_post(monkeypatch, payload={"success": True})

    result = whatsapp.create_group("Archery", ["12025551234"], community_jid="12025551234@s.whatsapp.net")

    assert result["success"] is False
    assert "community_jid" in result["message"]
    assert calls == []


def test_create_group_surfaces_bridge_error_json(monkeypatch):
    _capture_post(monkeypatch, payload={"success": False, "message": "create failed: boom"}, status_code=502)

    result = whatsapp.create_group("Trip", ["12025551234"])

    assert result == {"success": False, "message": "create failed: boom"}


def test_create_group_handles_non_json_error_from_old_bridge(monkeypatch):
    _capture_post(monkeypatch, payload=None, status_code=404, text="404 page not found")

    result = whatsapp.create_group("Trip", ["12025551234"])

    assert result["success"] is False
    assert "HTTP 404" in result["message"]


def test_create_group_handles_request_exception(monkeypatch):
    def boom(url, json, headers=None):
        raise whatsapp.requests.ConnectionError("down")

    monkeypatch.setattr(whatsapp.requests, "post", boom)

    result = whatsapp.create_group("Trip", ["12025551234"])

    assert result["success"] is False
    assert "Request error" in result["message"]


# --- update_group_settings ------------------------------------------------


def test_update_group_settings_sends_only_provided_fields(monkeypatch):
    calls = _capture_post(monkeypatch, payload={"success": True, "group": {}})

    result = whatsapp.update_group_settings(
        "120363012345678901@g.us", name=" New name ", description="", announce=False, member_add="everyone"
    )

    assert result["success"] is True
    assert calls[0]["url"].endswith("/group/settings")
    assert calls[0]["json"] == {
        "group_jid": "120363012345678901@g.us",
        "name": "New name",
        "description": "",
        "announce": False,
        "member_add": "all_members",
    }


def test_update_group_settings_with_no_changes_still_calls_bridge(monkeypatch):
    calls = _capture_post(monkeypatch, payload={"success": True, "group": {"name": "Trip"}})

    result = whatsapp.update_group_settings("120363012345678901@g.us")

    assert result["group"] == {"name": "Trip"}
    assert calls[0]["json"] == {"group_jid": "120363012345678901@g.us"}


@pytest.mark.parametrize(
    ("group_jid", "kwargs", "expected"),
    [
        ("12025551234@s.whatsapp.net", {}, "@g.us"),
        ("", {}, "@g.us"),
        ("120363012345678901@g.us", {"name": "   "}, "name"),
        ("120363012345678901@g.us", {"name": "x" * 101}, "100 characters"),
        ("120363012345678901@g.us", {"member_add": "nobody"}, "member_add"),
    ],
)
def test_update_group_settings_validates_input(monkeypatch, group_jid, kwargs, expected):
    calls = _capture_post(monkeypatch, payload={"success": True})

    result = whatsapp.update_group_settings(group_jid, **kwargs)

    assert result["success"] is False
    assert expected in result["message"]
    assert calls == []


# --- update_group_participants --------------------------------------------


def test_update_group_participants_posts_normalized_action(monkeypatch):
    payload = {"success": True, "action": "add", "results": [{"jid": "12025551234@s.whatsapp.net", "ok": True}]}
    calls = _capture_post(monkeypatch, payload=payload)

    result = whatsapp.update_group_participants("120363012345678901@g.us", ["12025551234", ""], " Add ")

    assert result["success"] is True
    assert result["results"][0]["ok"] is True
    assert calls[0]["url"].endswith("/group/participants")
    assert calls[0]["json"] == {
        "group_jid": "120363012345678901@g.us",
        "participants": ["12025551234"],
        "action": "add",
    }


@pytest.mark.parametrize(
    ("group_jid", "participants", "action", "expected"),
    [
        ("12025551234@s.whatsapp.net", ["12025551234"], "add", "@g.us"),
        ("120363012345678901@g.us", ["12025551234"], "kick", "action"),
        ("120363012345678901@g.us", [], "add", "participant"),
    ],
)
def test_update_group_participants_validates_input(monkeypatch, group_jid, participants, action, expected):
    calls = _capture_post(monkeypatch, payload={"success": True})

    result = whatsapp.update_group_participants(group_jid, participants, action)

    assert result["success"] is False
    assert expected in result["message"]
    assert calls == []


# --- get_profile_picture --------------------------------------------------


def test_get_profile_picture_gets_with_preview_and_auth(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")
    payload = {"success": True, "jid": "12025551234@s.whatsapp.net", "url": "https://cdn/pic.jpg", "type": "preview"}
    calls = _capture_get(monkeypatch, payload=payload)

    result = whatsapp.get_profile_picture(" 12025551234 ")

    assert result["url"] == "https://cdn/pic.jpg"
    assert result["success"] is True
    assert calls[0]["url"] == f"{whatsapp.WHATSAPP_API_BASE_URL}/profile-picture"
    assert calls[0]["params"] == {"jid": "12025551234", "preview": "1"}
    assert calls[0]["headers"] == {"Authorization": "Bearer test-token"}
    assert calls[0]["kwargs"]["timeout"] == 15


def test_get_profile_picture_full_size_and_no_picture(monkeypatch):
    calls = _capture_get(
        monkeypatch, payload={"success": True, "jid": "x@s.whatsapp.net", "url": None, "reason": "not_set"}
    )

    result = whatsapp.get_profile_picture("x@s.whatsapp.net", preview=False)

    assert calls[0]["params"]["preview"] == "0"
    assert result["success"] is True
    assert result["url"] is None
    assert result["reason"] == "not_set"


def test_get_profile_picture_tolerates_old_bridge_404(monkeypatch):
    _capture_get(monkeypatch, payload=None, status_code=404, text="404 page not found")

    result = whatsapp.get_profile_picture("12025551234")

    assert result["success"] is False
    assert result["url"] is None
    assert result["jid"] == "12025551234"
    assert "HTTP 404" in result["message"]


def test_get_profile_picture_requires_jid(monkeypatch):
    calls = _capture_get(monkeypatch, payload={"success": True})

    result = whatsapp.get_profile_picture("  ")

    assert result["success"] is False
    assert result["url"] is None
    assert calls == []


# --- resolve_contacts -----------------------------------------------------


CONTACTS = {
    "harry": [
        {"jid": "15550000001@s.whatsapp.net", "name": "Harry Potter", "phone_number": "15550000001"},
        {"jid": "15550000002@s.whatsapp.net", "name": "Prince Harry", "phone_number": "15550000002"},
        {"jid": "15550000003@s.whatsapp.net", "name": "Harry", "phone_number": "15550000003"},
        {"jid": "15550000004@s.whatsapp.net", "name": "Harriet Jones", "phone_number": "15550000004"},
    ],
    "sam jones": [
        {"jid": "15550000010@s.whatsapp.net", "name": "Sam Jones", "phone_number": "15550000010"},
    ],
    "nobody": [],
}


def _install_search(monkeypatch, contacts=CONTACTS):
    monkeypatch.setattr(whatsapp, "search_contacts", lambda q: list(contacts.get(q.lower(), [])))


def test_resolve_contacts_classifies_unique_ambiguous_and_none(monkeypatch):
    _install_search(monkeypatch)
    monkeypatch.setattr(whatsapp, "_resolve_lid_to_phone", lambda jid: None)
    avatar_calls = []

    def fake_pic(jid, preview=True, timeout=15):
        avatar_calls.append(jid)
        return {"success": True, "jid": jid, "url": f"https://cdn/{jid.split('@')[0]}.jpg"}

    monkeypatch.setattr(whatsapp, "get_profile_picture", fake_pic)

    results = whatsapp.resolve_contacts(["Harry", "Sam Jones", "nobody", ""])

    assert [r["query"] for r in results] == ["Harry", "Sam Jones", "nobody"]
    harry, sam, nobody = results

    assert harry["status"] == "ambiguous"
    assert harry["total_matches"] == 4
    # Exact name first, then prefix match, then whole-word match, then anything else.
    assert [m["name"] for m in harry["matches"]] == ["Harry", "Harry Potter", "Prince Harry", "Harriet Jones"]
    assert harry["matches"][0] == {
        "jid": "15550000003@s.whatsapp.net",
        "name": "Harry",
        "phone": "15550000003",
        "avatar_url": "https://cdn/15550000003.jpg",
    }

    assert sam["status"] == "unique"
    assert sam["matches"] == [
        {"jid": "15550000010@s.whatsapp.net", "name": "Sam Jones", "phone": "15550000010", "avatar_url": None}
    ]

    assert nobody == {"query": "nobody", "status": "none", "matches": [], "total_matches": 0}

    # Avatars are fetched only for the ambiguous name.
    assert sorted(avatar_calls) == sorted(m["jid"] for m in harry["matches"])


def test_resolve_contacts_caps_matches_and_only_fetches_shown_avatars(monkeypatch):
    many = {"x": [{"jid": f"1555{i:07d}@s.whatsapp.net", "name": f"X{i}", "phone_number": "p"} for i in range(8)]}
    _install_search(monkeypatch, many)
    monkeypatch.setattr(whatsapp, "_resolve_lid_to_phone", lambda jid: None)
    avatar_calls = []
    monkeypatch.setattr(
        whatsapp,
        "get_profile_picture",
        lambda jid, preview=True, timeout=15: avatar_calls.append(jid) or {"success": True, "url": None},
    )

    (result,) = whatsapp.resolve_contacts(["x"], max_matches=3)

    assert result["status"] == "ambiguous"
    assert result["total_matches"] == 8
    assert len(result["matches"]) == 3
    assert len(avatar_calls) == 3


def test_resolve_contacts_tolerates_avatar_failures_and_can_skip_them(monkeypatch):
    _install_search(monkeypatch)
    monkeypatch.setattr(whatsapp, "_resolve_lid_to_phone", lambda jid: None)
    monkeypatch.setattr(
        whatsapp,
        "get_profile_picture",
        lambda jid, preview=True, timeout=15: {"success": False, "url": None, "message": "HTTP 404"},
    )

    (result,) = whatsapp.resolve_contacts(["Harry"])
    assert result["status"] == "ambiguous"
    assert all(m["avatar_url"] is None for m in result["matches"])

    def must_not_call(*args, **kwargs):
        raise AssertionError("avatars should not be fetched when with_avatars=False")

    monkeypatch.setattr(whatsapp, "get_profile_picture", must_not_call)
    (result,) = whatsapp.resolve_contacts(["Harry"], with_avatars=False)
    assert result["status"] == "ambiguous"


def test_resolve_contacts_merges_lid_and_phone_rows_for_same_person(monkeypatch):
    contacts = {
        "vicky": [
            {"jid": "184125298348272@lid", "name": "Vicky", "phone_number": "184125298348272"},
            {"jid": "15550000020@s.whatsapp.net", "name": "", "phone_number": "15550000020"},
            {"jid": "999@lid", "name": "Vicky Two", "phone_number": "999"},
            {"jid": "120363012345678901@g.us", "name": "Vicky fan club", "phone_number": "120363012345678901"},
        ]
    }
    _install_search(monkeypatch, contacts)
    monkeypatch.setattr(
        whatsapp, "_resolve_lid_to_phone", lambda jid: "15550000020" if jid.startswith("184125298348272") else None
    )
    monkeypatch.setattr(
        whatsapp, "get_profile_picture", lambda jid, preview=True, timeout=15: {"success": True, "url": None}
    )

    (result,) = whatsapp.resolve_contacts(["Vicky"])

    # Group dropped; resolved LID row canonicalised onto the phone JID and merged with the phone row.
    assert result["total_matches"] == 2
    assert result["matches"][0] == {
        "jid": "15550000020@s.whatsapp.net",
        "name": "Vicky",
        "phone": "15550000020",
        "avatar_url": None,
    }
    assert result["matches"][1]["jid"] == "999@lid"
    assert result["matches"][1]["phone"] is None


# --- invite links ---------------------------------------------------------


def test_get_group_invite_link_reads_by_default_and_resets_on_request(monkeypatch):
    payload = {"success": True, "link": "https://chat.whatsapp.com/AbC", "code": "AbC"}
    calls = _capture_post(monkeypatch, payload=payload)

    result = whatsapp.get_group_invite_link(" 120363012345678901@g.us ")
    whatsapp.get_group_invite_link("120363012345678901@g.us", reset=True)

    assert result["link"] == "https://chat.whatsapp.com/AbC"
    assert calls[0]["url"].endswith("/group/invite-link")
    assert calls[0]["json"] == {"group_jid": "120363012345678901@g.us"}
    assert calls[1]["json"] == {"group_jid": "120363012345678901@g.us", "reset": True}


def test_get_group_invite_link_validates_and_surfaces_bridge_errors(monkeypatch):
    calls = _capture_post(
        monkeypatch,
        payload={"success": False, "message": "only group admins can see or reset the invite link"},
        status_code=403,
    )

    bad = whatsapp.get_group_invite_link("12025551234@s.whatsapp.net")
    assert bad["success"] is False and "group_jid" in bad["message"]
    assert calls == []

    denied = whatsapp.get_group_invite_link("120363012345678901@g.us")
    assert denied["success"] is False
    assert "only group admins" in denied["message"]


# --- communities ----------------------------------------------------------


def test_list_communities_gets_with_auth_and_sub_group_flag(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "tok")
    community = {"jid": "100@g.us", "name": "Forest", "sub_groups": []}
    calls = _capture_get(monkeypatch, payload={"success": True, "communities": [community]})

    result = whatsapp.list_communities()
    whatsapp.list_communities(include_sub_groups=False)

    assert result["communities"] == [community]
    assert calls[0]["url"].endswith("/communities")
    assert [c["params"] for c in calls] == [{"sub_groups": "1"}, {"sub_groups": "0"}]
    assert calls[0]["headers"] == {"Authorization": "Bearer tok"}
    assert calls[0]["kwargs"]["timeout"] > 40  # above the bridge's own 40s budget


def test_list_communities_reports_old_bridge_and_keeps_shape(monkeypatch):
    _capture_get(monkeypatch, payload=None, status_code=404, text="404 page not found")

    result = whatsapp.list_communities()

    assert result["success"] is False
    assert "404" in result["message"]
    assert result["communities"] == []


def test_create_community_posts_name_and_optional_description(monkeypatch):
    calls = _capture_post(monkeypatch, payload={"success": True, "community": {"jid": "100@g.us"}})

    result = whatsapp.create_community("  Forest  ", description="  Woods ")
    whatsapp.create_community("Forest", description="   ")

    assert result["community"]["jid"] == "100@g.us"
    assert calls[0]["url"].endswith("/community/create")
    assert calls[0]["json"] == {"name": "Forest", "description": "Woods"}
    assert calls[1]["json"] == {"name": "Forest"}


@pytest.mark.parametrize(
    ("name", "expected"), [("  ", "Community name must be provided"), ("x" * 101, "100 characters")]
)
def test_create_community_validates_name(monkeypatch, name, expected):
    calls = _capture_post(monkeypatch, payload={"success": True})

    result = whatsapp.create_community(name)

    assert result["success"] is False
    assert expected in result["message"]
    assert calls == []


def test_update_community_groups_posts_normalized_action(monkeypatch):
    calls = _capture_post(monkeypatch, payload={"success": True, "results": []})

    whatsapp.update_community_groups(" 100@g.us ", ["101@g.us", " 102@g.us "], " LINK ")
    whatsapp.update_community_groups("100@g.us", "101@g.us", "unlink")

    assert calls[0]["url"].endswith("/community/groups")
    assert calls[0]["json"] == {"community_jid": "100@g.us", "group_jids": ["101@g.us", "102@g.us"], "action": "link"}
    assert calls[1]["json"] == {"community_jid": "100@g.us", "group_jids": ["101@g.us"], "action": "unlink"}


@pytest.mark.parametrize(
    ("community_jid", "group_jids", "action", "expected"),
    [
        ("12025551234@s.whatsapp.net", ["101@g.us"], "link", "community_jid"),
        ("100@g.us", ["101@g.us"], "move", "link, unlink"),
        ("100@g.us", [], "link", "At least one group"),
        ("100@g.us", ["101@g.us", "12025551234"], "link", "12025551234"),
        ("100@g.us", ["100@g.us"], "unlink", "community itself"),
    ],
)
def test_update_community_groups_validates_input(monkeypatch, community_jid, group_jids, action, expected):
    calls = _capture_post(monkeypatch, payload={"success": True})

    result = whatsapp.update_community_groups(community_jid, group_jids, action)

    assert result["success"] is False
    assert expected in result["message"]
    assert calls == []


# --- MCP tool wrappers ----------------------------------------------------


def test_mcp_tools_delegate_with_keyword_arguments(monkeypatch):
    seen = {}
    monkeypatch.setattr(mcp_main, "whatsapp_create_group", lambda *a, **k: seen.setdefault("create", (a, k)) or {})
    monkeypatch.setattr(
        mcp_main, "whatsapp_update_group_settings", lambda *a, **k: seen.setdefault("settings", (a, k)) or {}
    )
    monkeypatch.setattr(
        mcp_main, "whatsapp_update_group_participants", lambda *a, **k: seen.setdefault("participants", (a, k)) or {}
    )
    monkeypatch.setattr(mcp_main, "whatsapp_get_profile_picture", lambda *a, **k: seen.setdefault("pic", (a, k)) or {})
    monkeypatch.setattr(mcp_main, "whatsapp_resolve_contacts", lambda *a, **k: seen.setdefault("resolve", (a, k)) or [])
    monkeypatch.setattr(
        mcp_main, "whatsapp_list_communities", lambda *a, **k: seen.setdefault("communities", (a, k)) or {}
    )
    monkeypatch.setattr(
        mcp_main, "whatsapp_update_community_groups", lambda *a, **k: seen.setdefault("link", (a, k)) or {}
    )
    monkeypatch.setattr(
        mcp_main, "whatsapp_get_group_invite_link", lambda *a, **k: seen.setdefault("invite", (a, k)) or {}
    )
    monkeypatch.setattr(
        mcp_main, "whatsapp_create_community", lambda *a, **k: seen.setdefault("community", (a, k)) or {}
    )

    mcp_main.create_group("Trip", ["1"], announce=True, member_add="admins_only")
    mcp_main.update_group_settings("g@g.us", locked=True)
    mcp_main.update_group_participants("g@g.us", ["1"], "promote")
    mcp_main.get_profile_picture("1", preview=False)
    mcp_main.resolve_contacts(["Harry"], max_matches=2, with_avatars=False)
    mcp_main.list_communities(include_sub_groups=False)
    mcp_main.update_community_groups("100@g.us", ["101@g.us"], "link")
    mcp_main.get_group_invite_link("100@g.us")
    mcp_main.create_community("Forest")

    assert seen["create"] == (
        ("Trip", ["1"]),
        {
            "description": None,
            "announce": True,
            "locked": None,
            "member_add": "admins_only",
            "join_approval": None,
            "community_jid": None,
        },
    )
    assert seen["settings"] == (
        ("g@g.us",),
        {
            "name": None,
            "description": None,
            "announce": None,
            "locked": True,
            "member_add": None,
            "join_approval": None,
        },
    )
    assert seen["participants"] == (("g@g.us", ["1"], "promote"), {})
    assert seen["pic"] == (("1",), {"preview": False})
    assert seen["resolve"] == ((["Harry"],), {"max_matches": 2, "with_avatars": False})
    assert seen["communities"] == ((), {"include_sub_groups": False})
    assert seen["link"] == (("100@g.us", ["101@g.us"], "link"), {})
    assert seen["invite"] == (("100@g.us",), {"reset": False})
    assert seen["community"] == (("Forest",), {"description": None})
