"""Tests for the private people list (fork).

Everything runs against temporary files: a fake whatsmeow store, a fake messages.db and a
people list keyed from WHATSAPP_PEOPLE_KEY. Any real network request fails the test, and the
macOS Keychain is only ever reached through a mocked ``people._security``.
"""

import base64
import json
import os
import sqlite3
import stat
import subprocess
import sys

import pytest
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

import main as mcp_main
import people
import whatsapp

ALICE = "447700900001@s.whatsapp.net"
BOB = "447700900002@s.whatsapp.net"
CLEO = "447700900003@s.whatsapp.net"
DAN = "447700900004@s.whatsapp.net"
SELF = "447700900099"
BOB_LID = "184125298348272"
STRANGER = "447700900555@s.whatsapp.net"


def _make_whatsmeow_db(path):
    conn = sqlite3.connect(path)
    conn.executescript(
        """
        CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT,
                                         push_name TEXT, business_name TEXT);
        CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT);
        CREATE TABLE whatsmeow_device (jid TEXT, lid TEXT, platform TEXT);
        """
    )
    me = f"{SELF}@s.whatsapp.net"
    conn.executemany(
        "INSERT INTO whatsmeow_contacts VALUES (?, ?, ?, ?, ?, ?)",
        [
            (me, ALICE, "Alice", "Alice Archer", "Ali", None),
            (me, f"{BOB_LID}@lid", "Bob", "Bob Baker", None, None),
            (me, CLEO, "Cleo", "Cleo Carter", None, None),
            (me, DAN, None, "Dan <b>Dangerous</b>", None, None),
            (me, "447700900010@s.whatsapp.net", None, "Alice Other", None, None),
        ],
    )
    conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (BOB_LID, "447700900002"))
    conn.execute("INSERT INTO whatsmeow_device VALUES (?, ?, ?)", (f"{SELF}:12@s.whatsapp.net", "99999@lid", "x"))
    conn.commit()
    conn.close()


def _make_messages_db(path):
    conn = sqlite3.connect(path)
    conn.execute("CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)")
    conn.execute("INSERT INTO chats VALUES (?, ?, ?)", ("447700900020@s.whatsapp.net", "Eve from chat", None))
    conn.commit()
    conn.close()


@pytest.fixture(autouse=True)
def env(monkeypatch, tmp_path):
    wa_db = tmp_path / "whatsapp.db"
    msg_db = tmp_path / "messages.db"
    _make_whatsmeow_db(wa_db)
    _make_messages_db(msg_db)
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(wa_db))
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(msg_db))
    monkeypatch.setenv("WHATSAPP_PEOPLE_KEY", base64.b64encode(b"k" * 32).decode())
    monkeypatch.setenv("WHATSAPP_PEOPLE_DB_PATH", str(tmp_path / "data" / "people.sqlite.enc"))
    monkeypatch.setenv("WHATSAPP_PEOPLE_REVIEW_DIR", str(tmp_path / "review"))
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "tok")
    monkeypatch.setattr(people, "_AVATAR_PAUSE_S", 0)
    monkeypatch.setattr(people, "_exclude_from_backups", lambda path: None)
    people._key_cache.clear()

    def no_network(*args, **kwargs):
        raise AssertionError("a test tried to reach the network")

    monkeypatch.setattr(whatsapp.requests, "post", no_network)
    monkeypatch.setattr(whatsapp.requests, "get", no_network)

    def no_keychain(*args, **kwargs):
        raise AssertionError("a test tried to use the real Keychain")

    monkeypatch.setattr(people, "_security", no_keychain)
    monkeypatch.setattr(
        whatsapp,
        "get_profile_picture",
        lambda jid, preview=True: {"success": True, "url": f"https://pps.whatsapp.net/{jid}"},
    )
    yield tmp_path
    people._key_cache.clear()


def _add(jid, flavour="warm", **kw):
    result = people.add_person(jid=jid, flavour=flavour, **kw)
    assert result["success"], result
    return result["person"]


def _decrypted_image(tmp_path):
    blob = (tmp_path / "data" / "people.sqlite.enc").read_bytes()
    header = len(people._FILE_MAGIC) + 12
    return AESGCM(b"k" * 32).decrypt(blob[len(people._FILE_MAGIC) : header], blob[header:], people._FILE_MAGIC)


# --- Storage -----------------------------------------------------------------------


def test_list_file_is_encrypted_and_private(env):
    _add(ALICE, place="Stroud", tags=["climbing"])

    path = env / "data" / "people.sqlite.enc"
    raw = path.read_bytes()
    assert raw.startswith(people._FILE_MAGIC)
    for secret in (b"Alice", b"447700900001", b"Stroud", b"climbing", b"SQLite format"):
        assert secret not in raw
    image = _decrypted_image(env)
    assert image.startswith(b"SQLite format 3")
    assert b"Stroud" in image
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600
    assert stat.S_IMODE(os.stat(str(path) + ".lock").st_mode) == 0o600
    assert not [p for p in os.listdir(path.parent) if "tmp" in p or "journal" in p or "wal" in p]


def test_reading_before_anything_is_saved_creates_nothing(env):
    assert people.query_people()["people"] == []
    assert not (env / "data").exists()


def test_wrong_key_or_tampering_fails_cleanly(env, monkeypatch):
    _add(ALICE)
    monkeypatch.setenv("WHATSAPP_PEOPLE_KEY", base64.b64encode(b"x" * 32).decode())
    result = people.query_people()
    assert result["success"] is False and "Could not decrypt" in result["message"]

    monkeypatch.setenv("WHATSAPP_PEOPLE_KEY", base64.b64encode(b"k" * 32).decode())
    path = env / "data" / "people.sqlite.enc"
    blob = bytearray(path.read_bytes())
    blob[-1] ^= 1
    path.write_bytes(bytes(blob))
    result = people.query_people()
    assert result["success"] is False and "modified" in result["message"]


@pytest.mark.parametrize("bad", ["not base64!", base64.b64encode(b"short").decode()])
def test_bad_env_key_is_rejected(monkeypatch, bad):
    monkeypatch.setenv("WHATSAPP_PEOPLE_KEY", bad)
    result = people.add_person(jid=ALICE, flavour="warm")
    assert result["success"] is False and "WHATSAPP_PEOPLE_KEY" in result["message"]


def test_list_in_a_cloud_synced_folder_is_refused(monkeypatch):
    home = os.path.realpath(os.path.expanduser("~"))
    target = os.path.join(home, "Library", "Mobile Documents", "qx-test-never-created", "people.sqlite.enc")
    monkeypatch.setenv("WHATSAPP_PEOPLE_DB_PATH", target)
    result = people.add_person(jid=ALICE, flavour="warm")
    assert result["success"] is False and "synced" in result["message"]
    assert not os.path.exists(os.path.dirname(target))


def test_removed_people_leave_nothing_in_the_file(env):
    _add(ALICE, display_name="Zebedee Quux", place="Totnes", tags=["unique-tag-xyz"])
    _add(CLEO)
    assert b"Zebedee Quux" in _decrypted_image(env)

    assert people.remove_person(ALICE)["success"]

    image = _decrypted_image(env)
    for gone in (b"Zebedee Quux", b"Totnes", b"unique-tag-xyz", b"447700900001"):
        assert gone not in image


# --- Keychain -------------------------------------------------------------------------


class FakeKeychain:
    def __init__(self, stored=None, find_code=None):
        self.stored = stored
        self.find_code = find_code
        self.calls = []

    def __call__(self, args, stdin=None):
        self.calls.append((args, stdin))
        if args[0] == "find-generic-password":
            if self.find_code is not None:
                return subprocess.CompletedProcess(args, self.find_code, "", "User interaction is not allowed.")
            if self.stored:
                return subprocess.CompletedProcess(args, 0, self.stored + "\n", "")
            return subprocess.CompletedProcess(args, 44, "", "The specified item could not be found in the keychain.")
        if args == ["-i"]:
            self.stored = stdin.split(" -w ", 1)[1].strip()
            return subprocess.CompletedProcess(args, 0, "", "")
        raise AssertionError(args)


@pytest.fixture
def keychain(monkeypatch):
    monkeypatch.delenv("WHATSAPP_PEOPLE_KEY")
    monkeypatch.setattr(sys, "platform", "darwin")
    fake = FakeKeychain()
    monkeypatch.setattr(people, "_security", fake)
    return fake


def test_keychain_key_is_created_for_a_new_list_without_exposing_it(keychain):
    result = people.add_person(jid=ALICE, flavour="warm")

    assert result["success"], result
    assert "Created a new encryption key" in result["notes"][0]
    add = [c for c in keychain.calls if c[0] == ["-i"]]
    assert len(add) == 1
    assert keychain.stored not in " ".join(arg for call in keychain.calls for arg in call[0])
    assert f"-s {people.KEYCHAIN_SERVICE} -a {people.KEYCHAIN_ACCOUNT}" in add[0][1]
    assert len(base64.b64decode(keychain.stored)) == 32

    # A fresh process finds the stored key and opens the list without creating another.
    people._key_cache.clear()
    keychain.calls.clear()
    listed = people.query_people()
    assert listed["count"] == 1
    assert [c[0][0] for c in keychain.calls] == ["find-generic-password"]


def test_missing_key_for_an_existing_list_is_never_replaced(keychain):
    assert people.add_person(jid=ALICE, flavour="warm")["success"]
    people._key_cache.clear()
    keychain.stored = None
    keychain.calls.clear()

    result = people.add_person(jid=CLEO, flavour="warm")

    assert result["success"] is False and "key is missing" in result["message"]
    assert all(c[0] != ["-i"] for c in keychain.calls)


def test_unreadable_keychain_points_to_the_env_var(keychain):
    keychain.find_code = 36
    result = people.add_person(jid=ALICE, flavour="warm")
    assert result["success"] is False
    assert "could not be read" in result["message"] and "WHATSAPP_PEOPLE_KEY" in result["message"]
    assert all(c[0] != ["-i"] for c in keychain.calls)


# --- People -------------------------------------------------------------------------


def test_add_person_by_name_returns_candidates_with_avatars_and_saves_nothing(env):
    result = people.add_person(query="Alice")

    assert result["success"] and result["saved"] is False
    jids = [c["jid"] for c in result["candidates"]]
    assert ALICE in jids and "447700900010@s.whatsapp.net" in jids
    assert all(c["avatar_url"].startswith("https://pps.whatsapp.net/") for c in result["candidates"])
    assert all(c["already_on_list"] is False for c in result["candidates"])
    assert not (env / "data" / "people.sqlite.enc").exists()

    _add(ALICE)
    again = people.add_person(query="Alice Archer")
    assert next(c for c in again["candidates"] if c["jid"] == ALICE)["already_on_list"] is True


def test_add_person_by_phone_query(env):
    result = people.add_person(query="+44 7700 900003")
    assert [c["jid"] for c in result["candidates"]] == [CLEO]


def test_add_person_saves_confirmed_contact_with_defaults(env):
    person = _add(ALICE, place="  Stroud ", tags=["Birthdays Only", "climbing", "climbing"])
    assert person == {
        "jid": ALICE,
        "phone": "447700900001",
        "display_name": "Alice Archer",
        "flavour": "warm",
        "place": "Stroud",
        "tags": ["birthdays-only", "climbing"],
    }


def test_add_person_accepts_lid_and_phone_forms_and_known_chats(env):
    bob = _add(f"{BOB_LID}@lid", flavour="hot", display_name="Bob")
    assert bob["jid"] == BOB
    eve = _add("+44 7700 900020")  # only known from a chat in messages.db
    assert eve["display_name"] == "Eve from chat"
    dup = people.add_person(jid="447700900002", flavour="cool")
    assert dup["success"] is False and "already on the list" in dup["message"]


@pytest.mark.parametrize(
    ("kwargs", "expected"),
    [
        ({"jid": STRANGER, "flavour": "warm"}, "not a contact"),
        ({"jid": ALICE}, "flavour must be"),
        ({"jid": ALICE, "flavour": "lukewarm"}, "flavour must be"),
        ({"jid": "120363012345678901@g.us", "flavour": "warm"}, "not a person"),
        ({"jid": "07700 900001", "flavour": "warm"}, "country code"),
        ({"jid": ALICE, "flavour": "warm", "tags": ["x" * 41]}, "longer than"),
        ({"jid": ALICE, "flavour": "warm", "place": "p" * 81}, "80 characters"),
        ({}, "Give query"),
    ],
)
def test_add_person_validation(kwargs, expected):
    result = people.add_person(**kwargs)
    assert result["success"] is False
    assert expected in result["message"]


def test_update_person(env):
    _add(ALICE, place="Stroud", tags=["climbing", "uni"])

    result = people.update_person(
        "447700900001", flavour="hot", place="Bristol", add_tags=["Book Club"], remove_tags=["uni"]
    )
    assert result["person"]["flavour"] == "hot"
    assert result["person"]["place"] == "Bristol"
    assert result["person"]["tags"] == ["book-club", "climbing"]
    assert people.update_person(ALICE, place="")["person"]["place"] == ""
    assert people.update_person(ALICE)["success"] is False
    assert people.update_person(CLEO, flavour="hot")["success"] is False  # not on the list


def test_remove_person_drops_event_status(env):
    _add(ALICE)
    event = people.create_event("Party", "2026-12-06")["event"]
    people.mark_attended(event["id"], [ALICE])
    result = people.remove_person(ALICE)
    assert result["success"] and "1 event" in result["message"]
    assert people.event_report(event["id"], sync=False)["counts"]["attended"] == 0
    assert people.remove_person(ALICE)["success"] is False


def _seed():
    _add(ALICE, flavour="hot", place="Stroud", tags=["climbing", "uni"])
    _add(BOB, flavour="warm", place="bristol", tags=["climbing"], display_name="Bob")
    _add(CLEO, flavour="cool", place="London", tags=["uni", "birthdays-only"])
    _add(DAN, flavour="warm", place="Totnes", tags=["birthdays-only"])


@pytest.mark.parametrize(
    ("filters", "expected"),
    [
        ({}, [ALICE, BOB, CLEO, DAN]),
        ({"min_flavour": "warm"}, [ALICE, BOB, DAN]),
        ({"min_flavour": "hot"}, [ALICE]),
        ({"places": ["BRISTOL", "stroud"]}, [ALICE, BOB]),
        ({"tags_include": ["climbing", "uni"]}, [ALICE]),
        ({"tags_any": ["uni", "birthdays-only"]}, [ALICE, CLEO, DAN]),
        ({"tags_exclude": ["birthdays-only"]}, [ALICE, BOB]),
        ({"min_flavour": "warm", "tags_exclude": ["Birthdays Only"], "places": ["Stroud"]}, [ALICE]),
    ],
)
def test_query_people_filters(filters, expected):
    _seed()
    result = people.query_people(**filters)
    assert result["success"], result
    assert sorted(p["jid"] for p in result["people"]) == sorted(expected)
    assert result["count"] == len(expected)


def test_query_people_rejects_bad_filters():
    assert people.query_people(min_flavour="tepid")["success"] is False
    assert people.query_people(event_statuses=["attended"])["success"] is False
    assert people.query_people(event_id=1, event_statuses=["maybe"])["success"] is False
    assert "no event 7" in people.query_people(event_id=7)["message"]


# --- Events -------------------------------------------------------------------------


def test_create_event_validation():
    assert "YYYY-MM-DD" in people.create_event("Party", "6 Dec")["message"]
    assert "@g.us" in people.create_event("Party", "2026-12-06", group_jid="12345")["message"]
    assert "name is required" in people.create_event("  ", "2026-12-06")["message"]
    event = people.create_event(" Winter  party ", "2026-12-06", group_jid="120363012345678901@g.us")["event"]
    assert event["name"] == "Winter party" and event["group_jid"] == "120363012345678901@g.us"


def test_hand_marks_and_status_precedence():
    _seed()
    eid = people.create_event("Party", "2026-12-06")["event"]["id"]

    result = people.mark_attended(eid, [ALICE, "447700900002", STRANGER])
    assert result["success"] is False  # one of three failed
    assert [r["ok"] for r in result["results"]] == [True, True, False]
    people.mark_attended(eid, [BOB], status="declined")
    assert people.mark_attended(eid, [ALICE], status="maybe")["success"] is False

    report = people.event_report(eid, sync=False)
    assert report["counts"] == {"invited": 0, "joined": 0, "attended": 1, "declined": 1}
    assert [p["jid"] for p in report["people"]["declined"]] == [BOB]
    none = people.query_people(event_id=eid, event_statuses=["none"])
    assert sorted(p["jid"] for p in none["people"]) == [CLEO, DAN]
    attended = people.query_people(event_id=eid, event_statuses=["attended"])["people"]
    assert attended[0]["event_status"] == "attended"


def _group(*participants):
    return {"success": True, "group": {"jid": "120363012345678901@g.us", "participants": list(participants)}}


def test_sync_event_group_marks_joined_and_leavers(monkeypatch):
    _seed()
    eid = people.create_event("Party", "2026-12-06")["event"]["id"]
    assert "no linked group" in people.sync_event_group(eid)["message"]

    members = [
        {"jid": ALICE, "phone": "447700900001", "lid": None},
        {"jid": f"{BOB_LID}@lid", "phone": None, "lid": BOB_LID},  # LID-addressed, resolved via the map
        {"jid": f"{SELF}@s.whatsapp.net", "phone": SELF, "lid": None},  # you
        {"jid": STRANGER, "phone": "447700900555", "lid": None},
    ]
    seen = []
    monkeypatch.setattr(whatsapp, "update_group_settings", lambda jid: seen.append(jid) or _group(*members))

    result = people.sync_event_group(eid, group_jid="120363012345678901@g.us")

    assert result["success"], result
    assert seen == ["120363012345678901@g.us"]
    assert sorted(p["jid"] for p in result["newly_joined"]) == [ALICE, BOB]
    assert result["not_on_list_count"] == 1 and result["not_on_list_phones"] == ["447700900555"]

    people.mark_attended(eid, [ALICE])
    monkeypatch.setattr(whatsapp, "update_group_settings", lambda jid: _group(members[0]))
    again = people.sync_event_group(eid)
    assert [p["jid"] for p in again["left_group"]] == [BOB]
    report = people.event_report(eid, sync=False)
    assert report["counts"]["attended"] == 1  # a hand mark survives the sync
    assert report["counts"]["joined"] == 0
    assert report["event"]["group_synced_at"]


def test_sync_event_group_reports_bridge_failure(monkeypatch):
    eid = people.create_event("Party", "2026-12-06", group_jid="120363012345678901@g.us")["event"]["id"]
    monkeypatch.setattr(
        whatsapp, "update_group_settings", lambda jid: {"success": False, "message": "Request error: refused"}
    )
    result = people.sync_event_group(eid)
    assert result["success"] is False and "refused" in result["message"]


# --- Send-job integration -----------------------------------------------------------


@pytest.fixture
def bridge(monkeypatch):
    calls = []
    state = {"job": {"id": 9, "status": "running", "dry_run": False}}

    class Resp:
        status_code = 200
        text = "OK"

        def json(self):
            return {"success": True, "message": "created", "job": state["job"]}

    def fake_post(url, json, headers=None, **kwargs):
        calls.append({"url": url, "json": json})
        return Resp()

    monkeypatch.setattr(whatsapp.requests, "post", fake_post)
    return calls, state


def test_start_send_job_from_filter_records_invites_on_the_event(bridge, monkeypatch):
    calls, _ = bridge
    _seed()
    eid = people.create_event("Winter party", "2026-12-06")["event"]["id"]

    result = people.start_send_job(
        "Hi {name}, party on 6 Dec!",
        people_filter={"min_flavour": "warm", "tags_exclude": ["birthdays-only"]},
        event_id=eid,
    )

    assert result["success"], result
    assert result["people_selected"] == 2 and result["event_id"] == eid
    assert calls[0]["url"].endswith("/send-jobs")
    assert calls[0]["json"] == {
        "recipients": [{"recipient": ALICE, "name": "Alice Archer"}, {"recipient": BOB, "name": "Bob"}],
        "template": "Hi {name}, party on 6 Dec!",
        "label": "Winter party",
    }

    # The bridge reports what it has sent so far; reading the event records those invites.
    sent_job = {
        "id": 9,
        "status": "running",
        "dry_run": False,
        "counts": {"sent": 1},
        "recipients": [{"jid": ALICE, "status": "sent", "sent_at": "2026-11-26T10:00:00+00:00"}],
    }
    asked = []
    monkeypatch.setattr(
        whatsapp,
        "get_send_job",
        lambda job_id, recipient_status=None: asked.append(job_id) or {"success": True, "job": sent_job},
    )
    report = people.event_report(eid)
    assert asked == [9]
    assert report["counts"]["invited"] == 1
    assert report["people"]["invited"][0]["invited_at"] == "2026-11-26T10:00:00+00:00"
    assert report["send_jobs"][0]["id"] == 9
    invited = people.query_people(event_id=eid, event_statuses=["invited"])["people"]
    assert [p["jid"] for p in invited] == [ALICE]


def test_start_send_job_merges_explicit_recipients_and_fills_names(bridge):
    calls, _ = bridge
    _seed()
    eid = people.create_event("Party", "2026-12-06")["event"]["id"]

    result = people.start_send_job(
        "Hi {name}", recipients=["447700900003", ALICE], people_filter={"min_flavour": "hot"}, event_id=eid
    )

    assert result["success"], result
    assert calls[0]["json"]["recipients"] == [
        {"recipient": "447700900003", "name": "Cleo Carter"},
        {"recipient": ALICE, "name": "Alice Archer"},
    ]


def test_start_send_job_refusals(bridge):
    calls, _ = bridge
    _seed()
    eid = people.create_event("Party", "2026-12-06")["event"]["id"]

    assert "matched nobody" in people.start_send_job("Hi", people_filter={"places": ["Leeds"]})["message"]
    assert "Unknown people_filter" in people.start_send_job("Hi", people_filter={"flavour": "hot"})["message"]
    assert "must be on the people list" in people.start_send_job("Hi", recipients=[STRANGER], event_id=eid)["message"]
    assert "Give recipients" in people.start_send_job("Hi")["message"]
    assert "no event 99" in people.start_send_job("Hi", recipients=[ALICE], event_id=99)["message"]
    assert calls == []


def test_dry_run_jobs_are_not_recorded_on_the_event(bridge, monkeypatch):
    calls, state = bridge
    _seed()
    eid = people.create_event("Party", "2026-12-06")["event"]["id"]
    state["job"] = {"id": 10, "status": "running", "dry_run": True}

    result = people.start_send_job("Hi", people_filter={"min_flavour": "hot"}, event_id=eid, dry_run=True)

    assert result["success"] and "Dry run" in result["event_note"]
    assert calls[0]["json"]["dry_run"] is True
    monkeypatch.setattr(whatsapp, "get_send_job", lambda *a, **k: pytest.fail("no linked jobs expected"))
    assert people.event_report(eid)["send_jobs"] == []


def test_plain_start_send_job_does_not_touch_the_people_list(bridge, env):
    calls, _ = bridge
    result = people.start_send_job("Hello", recipients=["447700900001"])
    assert result["success"] and calls[0]["json"]["recipients"] == [{"recipient": "447700900001"}]
    assert not (env / "data").exists()


# --- Review page ----------------------------------------------------------------------

JPEG = b"\xff\xd8\xff\xe0" + b"\x00" * 64


class FakeImage:
    def __init__(self, body, status=200):
        self.body, self.status_code = body, status

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def iter_content(self, size):
        yield self.body


def test_render_review_page(env, monkeypatch):
    _seed()
    fetched = []

    def fake_get(url, timeout=None, stream=None):
        fetched.append(url)
        return FakeImage(JPEG if ALICE in url else b"<html>not an image</html>")

    monkeypatch.setattr(whatsapp.requests, "get", fake_get)
    monkeypatch.setattr(
        whatsapp,
        "get_profile_picture",
        lambda jid, preview=True: {
            "success": True,
            "url": "https://evil.example/x.jpg" if jid == DAN else f"https://pps.whatsapp.net/{jid}",
        },
    )

    result = people.render_review_page(min_flavour="warm", title="Winter <party>")

    assert result["success"], result
    path = result["path"]
    assert os.path.dirname(path) == str(env / "review")
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600
    assert stat.S_IMODE(os.stat(env / "review").st_mode) == 0o700
    assert result["count"] == 3 and result["avatars_included"] == 1
    assert not any("evil.example" in u for u in fetched)  # only WhatsApp's CDN is fetched
    page = open(path, encoding="utf-8").read()
    assert "data:image/jpeg;base64," in page
    assert "Dan &lt;b&gt;Dangerous&lt;/b&gt;" in page and "<b>Dangerous" not in page
    assert "Winter &lt;party&gt;" in page
    assert "+447700900001" in page and "warm or above" in page
    assert "Cleo" not in page


def test_render_review_page_without_avatars_makes_no_lookups(env, monkeypatch):
    _add(ALICE)
    monkeypatch.setattr(whatsapp, "get_profile_picture", lambda *a, **k: pytest.fail("no lookups expected"))
    result = people.render_review_page(with_avatars=False)
    assert result["success"] and result["avatars_included"] == 0
    assert 'class="initials">AA<' in open(result["path"], encoding="utf-8").read()


# --- MCP tools --------------------------------------------------------------------------


async def test_people_tools_are_registered():
    tools = {t.name: t for t in await mcp_main.mcp.list_tools()}
    for name in (
        "add_person",
        "update_person",
        "remove_person",
        "query_people",
        "create_event",
        "sync_event_group",
        "mark_attended",
        "event_report",
        "render_review_page",
    ):
        assert name in tools, name

    def text(name):
        return " ".join(tools[name].description.split())

    assert "Two steps" in text("add_person") and "no notes field" in text("add_person")
    assert '"cool" < "warm" < "hot"' in text("add_person")
    start = tools["start_send_job"]
    assert start.inputSchema["required"] == ["message_template"]
    assert {"recipients", "people_filter", "event_id"} <= set(start.inputSchema["properties"])
    assert 'recorded as "invited"' in text("start_send_job")


async def test_query_people_tool_round_trip():
    _seed()
    result = await mcp_main.mcp.call_tool("query_people", {"min_flavour": "hot"})
    content = result[0] if isinstance(result, tuple) else result
    assert [p["jid"] for p in json.loads(content[0].text)["people"]] == [ALICE]
