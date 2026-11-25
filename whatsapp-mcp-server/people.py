"""Private people list for event invites (fork addition, see FORK.md).

A small, deliberately minimal list of people (WhatsApp JID, display name, flavour,
place, tags) and events (name, date, optional WhatsApp group, per-person status),
kept apart from the WhatsApp session and message databases.

Storage: one file, ``people.sqlite.enc`` next to the bridge's whatsapp.db (override
with WHATSAPP_PEOPLE_DB_PATH). It is a SQLite database image encrypted as a whole
with AES-256-GCM; it is only ever opened in memory, so no plaintext copy, WAL or
journal touches the disk. Every change rewrites the file atomically (after a VACUUM
with secure_delete on, so removed people leave nothing behind in the image), under
an flock so several MCP server processes can share it.

Key: WHATSAPP_PEOPLE_KEY (base64 of 32 bytes) if set, otherwise a generic password
in the macOS Keychain (service "qx-whatsapp-mcp-people", account "people-list").
A key is created in the Keychain only when no list exists yet; an existing list
whose key is missing is never replaced or opened in plaintext.

What is stored, and nothing else: per person the JID, a display name, a flavour
(cool < warm < hot), a free-text place and free-form tags; per event its name, date,
optional group JID and, per person, when they were invited, joined the group,
attended or declined. No notes, no phone books, no message content.
"""

from __future__ import annotations

import base64
import contextlib
import datetime as dt
import fcntl
import html
import os
import re
import sqlite3
import subprocess
import sys
import time
from collections.abc import Iterator
from typing import Any
from urllib.parse import urlparse

import requests
from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

import whatsapp

FLAVOURS = ("cool", "warm", "hot")  # ordered: cool < warm < hot
EVENT_STATUSES = ("invited", "joined", "attended", "declined")
HAND_MARKED_STATUSES = ("attended", "declined")
# "none" in a filter means "not on this event at all".
EVENT_FILTER_STATUSES = (*EVENT_STATUSES, "none")

KEYCHAIN_SERVICE = "qx-whatsapp-mcp-people"
KEYCHAIN_ACCOUNT = "people-list"
_FILE_MAGIC = b"QXPEOPLE\x01"
_NONCE_BYTES = 12
_SCHEMA_VERSION = 1
# errSecItemNotFound, as the exit status of `security find-generic-password`.
_KEYCHAIN_NOT_FOUND = 44

MAX_NAME_LENGTH = 100
MAX_PLACE_LENGTH = 80
MAX_TAG_LENGTH = 40
MAX_TAGS_PER_PERSON = 30
# Pause between profile-picture lookups: WhatsApp rate-limits bursts of IQs.
_AVATAR_PAUSE_S = 0.3
_AVATAR_MAX_BYTES = 2 * 1024 * 1024
# A group's membership is re-read by event_report at most this often.
_GROUP_SYNC_MAX_AGE_S = 10 * 60

# Folders under $HOME that cloud services commonly sync. The list and review pages
# refuse to live there.
_CLOUD_SYNCED_DIRS = (
    "Library/Mobile Documents",
    "Library/CloudStorage",
    "iCloud Drive",
    "Dropbox",
    "Google Drive",
    "OneDrive",
    "Box",
    "Desktop",
    "Documents",
)

_SCHEMA = """
CREATE TABLE IF NOT EXISTS people (
    jid          TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    flavour      INTEGER NOT NULL CHECK (flavour IN (1, 2, 3)),
    place        TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS person_tags (
    jid TEXT NOT NULL REFERENCES people(jid) ON DELETE CASCADE,
    tag TEXT NOT NULL,
    PRIMARY KEY (jid, tag)
);
CREATE TABLE IF NOT EXISTS events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL,
    date            TEXT NOT NULL,
    group_jid       TEXT,
    group_synced_at TEXT
);
CREATE TABLE IF NOT EXISTS event_people (
    event_id    INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    jid         TEXT NOT NULL REFERENCES people(jid) ON DELETE CASCADE,
    invited_at  TEXT,
    joined_at   TEXT,
    attended_at TEXT,
    declined_at TEXT,
    PRIMARY KEY (event_id, jid)
);
CREATE TABLE IF NOT EXISTS event_send_jobs (
    event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    job_id   INTEGER NOT NULL,
    PRIMARY KEY (event_id, job_id)
);
"""


class PeopleError(Exception):
    """A problem to report to the caller as success=False with this message."""


def _failure(message: str, **extra: Any) -> dict[str, Any]:
    return {"success": False, "message": message, **extra}


def _now() -> str:
    return dt.datetime.now().astimezone().replace(microsecond=0).isoformat()


# --- Locations -----------------------------------------------------------------


def _store_dir() -> str:
    return os.path.dirname(os.path.abspath(whatsapp.WHATSMEOW_DB_PATH))


def people_db_path() -> str:
    custom = os.getenv("WHATSAPP_PEOPLE_DB_PATH", "").strip()
    return os.path.abspath(os.path.expanduser(custom or os.path.join(_store_dir(), "people.sqlite.enc")))


def review_dir() -> str:
    custom = os.getenv("WHATSAPP_PEOPLE_REVIEW_DIR", "").strip()
    return os.path.abspath(os.path.expanduser(custom or os.path.join(_store_dir(), "people-review")))


def _refuse_cloud_synced(path: str, setting: str) -> None:
    home = os.path.realpath(os.path.expanduser("~"))
    real = os.path.realpath(path)
    for sub in _CLOUD_SYNCED_DIRS:
        root = os.path.join(home, sub)
        if real == root or real.startswith(root + os.sep):
            raise PeopleError(
                f"{path} is inside {root}, which may be synced to the cloud. "
                f"Set {setting} to a folder that is not synced."
            )


def _exclude_from_backups(path: str) -> None:
    """Best effort: keep a folder of plaintext review pages out of Time Machine."""
    if sys.platform != "darwin":
        return
    try:
        subprocess.run(["/usr/bin/tmutil", "addexclusion", path], capture_output=True, timeout=10, check=False)
    except (OSError, subprocess.SubprocessError):
        pass


# --- Key ---------------------------------------------------------------------------

_key_cache: dict[str, bytes] = {}


def _decode_key(raw: str, source: str) -> bytes:
    try:
        key = base64.b64decode(raw.strip(), validate=True)
    except (ValueError, TypeError):
        raise PeopleError(f"{source} is not valid base64") from None
    if len(key) != 32:
        raise PeopleError(f"{source} must be the base64 of exactly 32 random bytes")
    return key


def _security(args: list[str], stdin: str | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["/usr/bin/security", *args], input=stdin, capture_output=True, text=True, timeout=15, check=False
    )


_KEY_HELP = (
    "If the MCP server runs as a background service it may not be able to read your login Keychain; "
    "then put the key in its environment as WHATSAPP_PEOPLE_KEY instead."
)


def _load_key(create: bool) -> tuple[bytes, str | None]:
    """Return (key, note). note is set when a new key was just created."""
    env = os.getenv("WHATSAPP_PEOPLE_KEY", "")
    if env.strip():
        return _decode_key(env, "WHATSAPP_PEOPLE_KEY"), None
    if "keychain" in _key_cache:
        return _key_cache["keychain"], None
    if sys.platform != "darwin":
        raise PeopleError("Set WHATSAPP_PEOPLE_KEY (base64 of 32 random bytes): the Keychain is only on macOS")
    lookup = ["find-generic-password", "-s", KEYCHAIN_SERVICE, "-a", KEYCHAIN_ACCOUNT, "-w"]
    try:
        found = _security(lookup)
    except (OSError, subprocess.SubprocessError) as e:
        raise PeopleError(f"Could not run the macOS security tool: {e}. {_KEY_HELP}") from None
    if found.returncode == 0 and found.stdout.strip():
        key = _decode_key(found.stdout, "The Keychain item")
        _key_cache["keychain"] = key
        return key, None
    if found.returncode != _KEYCHAIN_NOT_FOUND:
        detail = (found.stderr or "").strip() or f"exit status {found.returncode}"
        raise PeopleError(f"The macOS Keychain could not be read ({detail}). {_KEY_HELP}")
    if not create:
        raise PeopleError(
            f"The people list exists but its key is missing: there is no Keychain item "
            f"(service {KEYCHAIN_SERVICE}, account {KEYCHAIN_ACCOUNT}) and WHATSAPP_PEOPLE_KEY is not set. "
            "Restore the key from your backup; without it the list cannot be opened."
        )
    key = os.urandom(32)
    encoded = base64.b64encode(key).decode("ascii")
    # Through stdin rather than argv, so the key never shows up in `ps`.
    command = f"add-generic-password -s {KEYCHAIN_SERVICE} -a {KEYCHAIN_ACCOUNT} -w {encoded}\n"
    try:
        _security(["-i"], stdin=command)
        check = _security(lookup)
    except (OSError, subprocess.SubprocessError) as e:
        raise PeopleError(f"Could not store a new key in the macOS Keychain: {e}. {_KEY_HELP}") from None
    if check.returncode != 0 or check.stdout.strip() != encoded:
        raise PeopleError(f"Could not store a new key in the macOS Keychain. {_KEY_HELP}")
    _key_cache["keychain"] = key
    return key, (
        f"Created a new encryption key for the people list in the macOS Keychain (service {KEYCHAIN_SERVICE}). "
        "Back it up, for example in a password manager: without it the list cannot be opened."
    )


# --- Encrypted file ------------------------------------------------------------------


def _read_image(path: str, key: bytes) -> bytes:
    with open(path, "rb") as fh:
        blob = fh.read()
    header = len(_FILE_MAGIC) + _NONCE_BYTES
    if not blob.startswith(_FILE_MAGIC) or len(blob) < header + 16:
        raise PeopleError(f"{path} is not a people list file")
    nonce = blob[len(_FILE_MAGIC) : header]
    try:
        return AESGCM(key).decrypt(nonce, blob[header:], _FILE_MAGIC)
    except InvalidTag:
        raise PeopleError("Could not decrypt the people list: wrong key, or the file was modified") from None


def _write_image(path: str, key: bytes, data: bytes) -> None:
    nonce = os.urandom(_NONCE_BYTES)
    blob = _FILE_MAGIC + nonce + AESGCM(key).encrypt(nonce, data, _FILE_MAGIC)
    tmp = f"{path}.tmp-{os.getpid()}"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        view = memoryview(blob)
        while view:
            view = view[os.write(fd, view) :]
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(tmp, path)
    dir_fd = os.open(os.path.dirname(path), os.O_RDONLY)
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)


def _prepare(conn: sqlite3.Connection) -> None:
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA foreign_keys = ON")
    conn.execute("PRAGMA secure_delete = ON")
    version = conn.execute("PRAGMA user_version").fetchone()[0]
    if version > _SCHEMA_VERSION:
        raise PeopleError("The people list was written by a newer version of this server")
    conn.executescript(_SCHEMA)
    conn.execute(f"PRAGMA user_version = {_SCHEMA_VERSION}")


@contextlib.contextmanager
def _open(write: bool) -> Iterator[tuple[sqlite3.Connection, list[str]]]:
    """Open the list in memory. With write=True, changes are saved when the block succeeds."""
    path = people_db_path()
    _refuse_cloud_synced(path, "WHATSAPP_PEOPLE_DB_PATH")
    notes: list[str] = []
    if not write and not os.path.exists(path):
        conn = sqlite3.connect(":memory:")
        try:
            _prepare(conn)
            yield conn, notes
        finally:
            conn.close()
        return
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    lock_fd = os.open(path + ".lock", os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX if write else fcntl.LOCK_SH)
        exists = os.path.exists(path)
        key, note = _load_key(create=not exists)
        if note:
            notes.append(note)
        conn = sqlite3.connect(":memory:")
        try:
            if exists:
                conn.deserialize(_read_image(path, key))
            _prepare(conn)
            yield conn, notes
            if write:
                conn.commit()
                conn.execute("VACUUM")
                _write_image(path, key, conn.serialize())
        finally:
            conn.close()
    finally:
        os.close(lock_fd)


def _with_notes(result: dict[str, Any], notes: list[str]) -> dict[str, Any]:
    if notes:
        result["notes"] = notes
    return result


# --- Validation -------------------------------------------------------------------

_PHONE_PATTERN = re.compile(r"^\+?[\d\s().-]+$")


def _read_only_db(path: str) -> sqlite3.Connection | None:
    if not os.path.exists(path):
        return None
    try:
        return sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    except sqlite3.Error:
        return None


def _lid_for_phone(phone: str) -> str | None:
    conn = _read_only_db(whatsapp.WHATSMEOW_DB_PATH)
    if conn is None:
        return None
    try:
        row = conn.execute("SELECT lid FROM whatsmeow_lid_map WHERE pn = ? LIMIT 1", (phone,)).fetchone()
        return row[0] if row else None
    except sqlite3.Error:
        return None
    finally:
        conn.close()


def canonical_jid(raw: str) -> str:
    """Map a JID or phone number to the form the list stores: the phone JID when known."""
    raw = (raw or "").strip()
    if not raw:
        raise PeopleError("A JID or phone number is required")
    if "@" in raw:
        user, _, server = raw.partition("@")
        user = user.split(":", 1)[0]
        if not user.isdigit():
            raise PeopleError(f"{raw!r} is not a person's JID")
        if server == "s.whatsapp.net":
            return f"{user}@s.whatsapp.net"
        if server == "lid":
            phone = whatsapp._resolve_lid_to_phone(user)
            return f"{phone}@s.whatsapp.net" if phone else f"{user}@lid"
        raise PeopleError(f"{raw!r} is not a person (groups, broadcast lists and channels are not people)")
    if not _PHONE_PATTERN.match(raw):
        raise PeopleError(f"{raw!r} is not a phone number or JID")
    digits = re.sub(r"\D", "", raw)
    if digits.startswith("0") or not 7 <= len(digits) <= 15:
        raise PeopleError(f"Phone number {raw!r} needs its country code, e.g. 447700900123")
    return f"{digits}@s.whatsapp.net"


def _phone_of(jid: str) -> str | None:
    return jid.split("@", 1)[0] if jid.endswith("@s.whatsapp.net") else None


def contact_record(jid: str) -> dict[str, Any] | None:
    """The contact behind a canonical JID, or None if the bridge does not know this person.

    Known means: in whatsmeow's contact store (by phone or LID) or a chat in messages.db.
    Both databases are opened read-only.
    """
    forms = [jid]
    phone = _phone_of(jid)
    if phone:
        lid = _lid_for_phone(phone)
        if lid:
            forms.append(f"{lid}@lid")
    marks = ",".join("?" * len(forms))
    name = None
    found = False
    conn = _read_only_db(whatsapp.WHATSMEOW_DB_PATH)
    if conn is not None:
        try:
            for row in conn.execute(
                f"SELECT full_name, push_name, first_name, business_name FROM whatsmeow_contacts "
                f"WHERE their_jid IN ({marks})",
                forms,
            ):
                found = True
                name = name or next((v for v in row if v), None)
        except sqlite3.Error:
            pass
        finally:
            conn.close()
    conn = _read_only_db(whatsapp.MESSAGES_DB_PATH)
    if conn is not None:
        try:
            for (chat_name,) in conn.execute(f"SELECT name FROM chats WHERE jid IN ({marks})", forms):
                found = True
                name = name or chat_name or None
        except sqlite3.Error:
            pass
        finally:
            conn.close()
    if not found:
        return None
    return {"jid": jid, "name": name, "phone": phone}


def _clean_text(value: str | None, field: str, limit: int, required: bool = False) -> str:
    text = " ".join((value or "").split())
    if required and not text:
        raise PeopleError(f"{field} is required")
    if len(text) > limit:
        raise PeopleError(f"{field} must be {limit} characters or fewer")
    return text


def _flavour_value(flavour: str | None, field: str = "flavour") -> int:
    key = (flavour or "").strip().lower()
    if key not in FLAVOURS:
        raise PeopleError(f"{field} must be one of: cool, warm, hot")
    return FLAVOURS.index(key) + 1


def _clean_tag(tag: Any) -> str:
    text = "-".join(str(tag).strip().lower().split())
    if not text:
        raise PeopleError("Tags must not be empty")
    if len(text) > MAX_TAG_LENGTH:
        raise PeopleError(f"Tag {text!r} is longer than {MAX_TAG_LENGTH} characters")
    return text


def _clean_tags(tags: Any) -> list[str]:
    if tags is None:
        return []
    if isinstance(tags, str):
        tags = [tags]
    out: list[str] = []
    for tag in tags:
        clean = _clean_tag(tag)
        if clean not in out:
            out.append(clean)
    return out


def _clean_list(values: Any) -> list[str]:
    if values is None:
        return []
    if isinstance(values, str):
        values = [values]
    return [" ".join(str(v).split()) for v in values if str(v).strip()]


def _event_id(event_id: Any) -> int:
    if isinstance(event_id, bool):
        raise PeopleError("event_id must be a positive whole number")
    try:
        value = int(str(event_id).strip())
    except (TypeError, ValueError):
        raise PeopleError("event_id must be a positive whole number") from None
    if value <= 0:
        raise PeopleError("event_id must be a positive whole number")
    return value


# --- Reading --------------------------------------------------------------------------


def _person_rows(conn: sqlite3.Connection) -> dict[str, dict[str, Any]]:
    people: dict[str, dict[str, Any]] = {}
    for row in conn.execute("SELECT jid, display_name, flavour, place FROM people"):
        people[row["jid"]] = {
            "jid": row["jid"],
            "phone": _phone_of(row["jid"]),
            "display_name": row["display_name"],
            "flavour": FLAVOURS[row["flavour"] - 1],
            "place": row["place"],
            "tags": [],
        }
    for row in conn.execute("SELECT jid, tag FROM person_tags ORDER BY tag"):
        if row["jid"] in people:
            people[row["jid"]]["tags"].append(row["tag"])
    return people


def _event_status(row: sqlite3.Row | dict[str, Any] | None) -> str:
    """One status per person and event. Hand marks win over what was observed."""
    if row is None:
        return "none"
    for status in ("attended", "declined", "joined", "invited"):
        if row[f"{status}_at"]:
            return status
    return "none"


def _event_rows(conn: sqlite3.Connection, event_id: int) -> dict[str, sqlite3.Row]:
    return {
        row["jid"]: row
        for row in conn.execute(
            "SELECT jid, invited_at, joined_at, attended_at, declined_at FROM event_people WHERE event_id = ?",
            (event_id,),
        )
    }


def _load_event(conn: sqlite3.Connection, event_id: int) -> dict[str, Any]:
    row = conn.execute(
        "SELECT id, name, date, group_jid, group_synced_at FROM events WHERE id = ?", (event_id,)
    ).fetchone()
    if row is None:
        raise PeopleError(f"There is no event {event_id}; event_report() lists them")
    return dict(row)


def _get_person(conn: sqlite3.Connection, jid: str) -> dict[str, Any]:
    person = _person_rows(conn).get(jid)
    if person is None:
        raise PeopleError(f"{jid} is not on the people list")
    return person


_FILTER_KEYS = (
    "min_flavour",
    "places",
    "tags_include",
    "tags_any",
    "tags_exclude",
    "event_id",
    "event_statuses",
)


def _normalize_filter(
    min_flavour: str | None = None,
    places: Any = None,
    tags_include: Any = None,
    tags_any: Any = None,
    tags_exclude: Any = None,
    event_id: Any = None,
    event_statuses: Any = None,
) -> dict[str, Any]:
    f: dict[str, Any] = {
        "min_flavour": _flavour_value(min_flavour, "min_flavour") if min_flavour else None,
        "places": {p.casefold() for p in _clean_list(places)},
        "tags_include": set(_clean_tags(tags_include)),
        "tags_any": set(_clean_tags(tags_any)),
        "tags_exclude": set(_clean_tags(tags_exclude)),
        "event_id": _event_id(event_id) if event_id is not None else None,
        "event_statuses": set(),
    }
    for status in _clean_list(event_statuses):
        status = status.lower()
        if status not in EVENT_FILTER_STATUSES:
            raise PeopleError("event_statuses may contain: " + ", ".join(EVENT_FILTER_STATUSES))
        f["event_statuses"].add(status)
    if f["event_statuses"] and f["event_id"] is None:
        raise PeopleError("event_statuses needs an event_id")
    return f


def _filter_from_dict(people_filter: Any) -> dict[str, Any]:
    if not isinstance(people_filter, dict):
        raise PeopleError("people_filter must be an object with query_people's filter fields")
    unknown = sorted(set(people_filter) - set(_FILTER_KEYS))
    if unknown:
        raise PeopleError(f"Unknown people_filter field(s) {', '.join(unknown)}; use: {', '.join(_FILTER_KEYS)}")
    return _normalize_filter(**people_filter)


def _select(conn: sqlite3.Connection, f: dict[str, Any]) -> list[dict[str, Any]]:
    people = _person_rows(conn)
    event_rows: dict[str, sqlite3.Row] = {}
    if f["event_id"] is not None:
        _load_event(conn, f["event_id"])
        event_rows = _event_rows(conn, f["event_id"])
    out = []
    for person in people.values():
        tags = set(person["tags"])
        if f["min_flavour"] and FLAVOURS.index(person["flavour"]) + 1 < f["min_flavour"]:
            continue
        if f["places"] and person["place"].casefold() not in f["places"]:
            continue
        if not f["tags_include"] <= tags:
            continue
        if f["tags_any"] and not (f["tags_any"] & tags):
            continue
        if f["tags_exclude"] & tags:
            continue
        if f["event_id"] is not None:
            status = _event_status(event_rows.get(person["jid"]))
            wanted = f["event_statuses"] or set(EVENT_STATUSES)
            if status not in wanted:
                continue
            person = {**person, "event_status": status}
        out.append(person)
    out.sort(key=lambda p: (p["display_name"].casefold(), p["jid"]))
    return out


# --- People -----------------------------------------------------------------------------


def add_person(
    query: str | None = None,
    jid: str | None = None,
    flavour: str | None = None,
    display_name: str | None = None,
    place: str | None = None,
    tags: Any = None,
    max_candidates: int = 5,
) -> dict[str, Any]:
    """Find candidates for a name or phone (query), or save a confirmed person (jid)."""
    try:
        if not jid:
            return _candidates(query, max_candidates)
        canonical = canonical_jid(jid)
        contact = contact_record(canonical)
        if contact is None:
            raise PeopleError(
                f"{canonical} is not a contact or chat this WhatsApp account knows; "
                "use add_person(query=...) to find the right person"
            )
        flavour_value = _flavour_value(flavour)
        name = _clean_text(display_name or contact["name"], "display_name", MAX_NAME_LENGTH, required=True)
        place_text = _clean_text(place, "place", MAX_PLACE_LENGTH)
        tag_list = _clean_tags(tags)
        if len(tag_list) > MAX_TAGS_PER_PERSON:
            raise PeopleError(f"A person can have at most {MAX_TAGS_PER_PERSON} tags")
        with _open(write=True) as (conn, notes):
            if conn.execute("SELECT 1 FROM people WHERE jid = ?", (canonical,)).fetchone():
                raise PeopleError(f"{name} ({canonical}) is already on the list; use update_person")
            conn.execute(
                "INSERT INTO people (jid, display_name, flavour, place) VALUES (?, ?, ?, ?)",
                (canonical, name, flavour_value, place_text),
            )
            conn.executemany("INSERT INTO person_tags (jid, tag) VALUES (?, ?)", [(canonical, t) for t in tag_list])
            person = _get_person(conn, canonical)
        return _with_notes({"success": True, "saved": True, "message": f"Added {name}.", "person": person}, notes)
    except PeopleError as e:
        return _failure(str(e))


def _candidates(query: str | None, max_candidates: int) -> dict[str, Any]:
    query = (query or "").strip()
    if not query:
        raise PeopleError("Give query (a name or phone number) to find someone, or jid to save a confirmed person")
    search = re.sub(r"\D", "", query) if _PHONE_PATTERN.match(query) else query
    limit = max(1, min(int(max_candidates or 5), 10))
    resolved = whatsapp.resolve_contacts([search], max_matches=limit, with_avatars=False)
    matches = resolved[0]["matches"] if resolved else []
    with _open(write=False) as (conn, _):
        listed = set(_person_rows(conn))
    for i, match in enumerate(matches):
        if i:
            time.sleep(_AVATAR_PAUSE_S)
        pic = whatsapp.get_profile_picture(match["jid"], preview=True)
        match["avatar_url"] = pic.get("url") if pic.get("success") else None
        match["already_on_list"] = match["jid"] in listed
    total = resolved[0]["total_matches"] if resolved else 0
    if not matches:
        message = f"No contact matches {query!r}. Try another spelling or their phone number with country code."
    else:
        message = (
            f"{len(matches)} candidate(s) for {query!r}; nothing saved yet. Show them to the user (avatar_url is a "
            "short-lived picture link), and once they confirm, call add_person(jid=..., flavour=...)."
        )
    return {
        "success": True,
        "saved": False,
        "message": message,
        "candidates": matches,
        "total_matches": total,
    }


def update_person(
    jid: str,
    flavour: str | None = None,
    place: str | None = None,
    add_tags: Any = None,
    remove_tags: Any = None,
    display_name: str | None = None,
) -> dict[str, Any]:
    try:
        canonical = canonical_jid(jid)
        updates: dict[str, Any] = {}
        if flavour is not None:
            updates["flavour"] = _flavour_value(flavour)
        if place is not None:
            updates["place"] = _clean_text(place, "place", MAX_PLACE_LENGTH)
        if display_name is not None:
            updates["display_name"] = _clean_text(display_name, "display_name", MAX_NAME_LENGTH, required=True)
        to_add, to_remove = _clean_tags(add_tags), _clean_tags(remove_tags)
        if not updates and not to_add and not to_remove:
            raise PeopleError("Nothing to change: give flavour, place, display_name, add_tags or remove_tags")
        with _open(write=True) as (conn, notes):
            _get_person(conn, canonical)
            for column, value in updates.items():
                conn.execute(f"UPDATE people SET {column} = ? WHERE jid = ?", (value, canonical))
            conn.executemany(
                "INSERT OR IGNORE INTO person_tags (jid, tag) VALUES (?, ?)", [(canonical, t) for t in to_add]
            )
            conn.executemany("DELETE FROM person_tags WHERE jid = ? AND tag = ?", [(canonical, t) for t in to_remove])
            count = conn.execute("SELECT COUNT(*) FROM person_tags WHERE jid = ?", (canonical,)).fetchone()[0]
            if count > MAX_TAGS_PER_PERSON:
                raise PeopleError(f"A person can have at most {MAX_TAGS_PER_PERSON} tags")
            person = _get_person(conn, canonical)
        return _with_notes({"success": True, "message": f"Updated {person['display_name']}.", "person": person}, notes)
    except PeopleError as e:
        return _failure(str(e))


def remove_person(jid: str) -> dict[str, Any]:
    try:
        canonical = canonical_jid(jid)
        with _open(write=True) as (conn, notes):
            person = _get_person(conn, canonical)
            events = conn.execute("SELECT COUNT(*) FROM event_people WHERE jid = ?", (canonical,)).fetchone()[0]
            conn.execute("DELETE FROM people WHERE jid = ?", (canonical,))
        return _with_notes(
            {
                "success": True,
                "message": f"Removed {person['display_name']} and their status on {events} event(s).",
                "removed": {"jid": canonical, "display_name": person["display_name"]},
            },
            notes,
        )
    except PeopleError as e:
        return _failure(str(e))


def query_people(
    min_flavour: str | None = None,
    places: Any = None,
    tags_include: Any = None,
    tags_any: Any = None,
    tags_exclude: Any = None,
    event_id: Any = None,
    event_statuses: Any = None,
) -> dict[str, Any]:
    try:
        f = _normalize_filter(min_flavour, places, tags_include, tags_any, tags_exclude, event_id, event_statuses)
        warnings = _sync_invites(f["event_id"])[1] if f["event_id"] is not None else []
        with _open(write=False) as (conn, _):
            people = _select(conn, f)
        result: dict[str, Any] = {"success": True, "count": len(people), "people": people}
        if warnings:
            result["warnings"] = warnings
        return result
    except PeopleError as e:
        return _failure(str(e))


# --- Events --------------------------------------------------------------------------


def create_event(name: str, date: str, group_jid: str | None = None) -> dict[str, Any]:
    try:
        clean_name = _clean_text(name, "name", MAX_NAME_LENGTH, required=True)
        try:
            day = dt.date.fromisoformat((date or "").strip()).isoformat()
        except ValueError:
            raise PeopleError("date must be YYYY-MM-DD, e.g. 2026-12-06") from None
        group = _clean_group_jid(group_jid)
        with _open(write=True) as (conn, notes):
            cursor = conn.execute(
                "INSERT INTO events (name, date, group_jid) VALUES (?, ?, ?)", (clean_name, day, group)
            )
            event = _load_event(conn, cursor.lastrowid)
        return _with_notes({"success": True, "message": f"Created event {event['id']}.", "event": event}, notes)
    except PeopleError as e:
        return _failure(str(e))


def _clean_group_jid(group_jid: str | None) -> str | None:
    if group_jid is None or not str(group_jid).strip():
        return None
    group = str(group_jid).strip()
    if not group.endswith("@g.us") or not group[:-5].replace("-", "").isdigit():
        raise PeopleError("group_jid must be a group JID ending in @g.us")
    return group


def _own_users() -> set[str]:
    """Phone and LID user parts of the paired account, to leave yourself out of group syncs."""
    conn = _read_only_db(whatsapp.WHATSMEOW_DB_PATH)
    if conn is None:
        return set()
    users: set[str] = set()
    try:
        for row in conn.execute("SELECT * FROM whatsmeow_device"):
            for value in row:
                if isinstance(value, str) and "@" in value:
                    users.add(value.split("@", 1)[0].split(":", 1)[0])
    except sqlite3.Error:
        pass
    finally:
        conn.close()
    return users


def _participant_jid(p: dict[str, Any]) -> str | None:
    jid = str(p.get("jid") or "")
    phone = p.get("phone") or (jid.split("@", 1)[0] if jid.endswith("@s.whatsapp.net") else None)
    lid = p.get("lid") or (jid.split("@", 1)[0] if jid.endswith("@lid") else None)
    if not phone and lid:
        phone = whatsapp._resolve_lid_to_phone(str(lid))
    if phone:
        return f"{str(phone).split(':', 1)[0]}@s.whatsapp.net"
    if lid:
        return f"{str(lid).split(':', 1)[0]}@lid"
    return None


def sync_event_group(event_id: Any, group_jid: str | None = None) -> dict[str, Any]:
    """Mark listed people who are in the event's WhatsApp group as joined (and unmark leavers)."""
    try:
        eid = _event_id(event_id)
        group = _clean_group_jid(group_jid)
        notes: list[str] = []
        if group:
            with _open(write=True) as (conn, notes):
                _load_event(conn, eid)
                conn.execute("UPDATE events SET group_jid = ?, group_synced_at = NULL WHERE id = ?", (group, eid))
        with _open(write=False) as (conn, _):
            event = _load_event(conn, eid)
        if not event["group_jid"]:
            raise PeopleError(f"Event {eid} has no linked group; pass group_jid to link one")
        info = whatsapp.update_group_settings(event["group_jid"])  # no settings given: just reads the group
        if not info.get("success"):
            return _failure(f"Could not read group {event['group_jid']}: {info.get('message')}")
        own = _own_users()
        in_group: set[str] = set()
        for p in info.get("group", {}).get("participants", []):
            jid = _participant_jid(p)
            if jid and jid.split("@", 1)[0] not in own:
                in_group.add(jid)
        now = _now()
        with _open(write=True) as (conn, more_notes):
            notes += more_notes
            people = _person_rows(conn)
            rows = _event_rows(conn, eid)
            listed = in_group & set(people)
            newly = sorted(j for j in listed if not (rows.get(j) and rows[j]["joined_at"]))
            left = sorted(j for j, r in rows.items() if r["joined_at"] and j not in listed)
            for jid in newly:
                conn.execute(
                    "INSERT INTO event_people (event_id, jid, joined_at) VALUES (?, ?, ?) "
                    "ON CONFLICT (event_id, jid) DO UPDATE SET joined_at = excluded.joined_at",
                    (eid, jid, now),
                )
            conn.executemany(
                "UPDATE event_people SET joined_at = NULL WHERE event_id = ? AND jid = ?", [(eid, j) for j in left]
            )
            conn.execute("UPDATE events SET group_synced_at = ? WHERE id = ?", (now, eid))
        not_listed = sorted(in_group - set(people))

        def names(jids: list[str]) -> list[dict[str, Any]]:
            return [{"jid": j, "display_name": people[j]["display_name"]} for j in jids]

        return _with_notes(
            {
                "success": True,
                "message": f"{len(listed)} people from the list are in the group ({len(newly)} new, {len(left)} left).",
                "group_jid": event["group_jid"],
                "in_group": names(sorted(listed)),
                "newly_joined": names(newly),
                "left_group": names(left),
                "not_on_list_count": len(not_listed),
                "not_on_list_phones": [_phone_of(j) or j for j in not_listed],
                "synced_at": now,
            },
            notes,
        )
    except PeopleError as e:
        return _failure(str(e))


def mark_attended(event_id: Any, people: Any, status: str = "attended") -> dict[str, Any]:
    """Hand-mark people as attended (or declined) for an event."""
    try:
        eid = _event_id(event_id)
        status = (status or "").strip().lower()
        if status not in HAND_MARKED_STATUSES:
            raise PeopleError("status must be 'attended' or 'declined'")
        entries = _clean_list(people)
        if not entries:
            raise PeopleError("Give at least one person (JID or phone number)")
        other = "declined" if status == "attended" else "attended"
        now = _now()
        results = []
        with _open(write=True) as (conn, notes):
            _load_event(conn, eid)
            listed = _person_rows(conn)
            for entry in entries:
                try:
                    jid = canonical_jid(entry)
                    if jid not in listed:
                        raise PeopleError(f"{jid} is not on the people list")
                except PeopleError as e:
                    results.append({"person": entry, "ok": False, "error": str(e)})
                    continue
                conn.execute(
                    f"INSERT INTO event_people (event_id, jid, {status}_at) VALUES (?, ?, ?) "
                    f"ON CONFLICT (event_id, jid) DO UPDATE SET {status}_at = excluded.{status}_at, {other}_at = NULL",
                    (eid, jid, now),
                )
                results.append({"person": entry, "jid": jid, "display_name": listed[jid]["display_name"], "ok": True})
        ok = sum(r["ok"] for r in results)
        return _with_notes(
            {
                "success": ok == len(results),
                "message": f"Marked {ok} of {len(results)} as {status}.",
                "results": results,
            },
            notes,
        )
    except PeopleError as e:
        return _failure(str(e))


def _linked_jobs(event_id: int) -> list[int]:
    with _open(write=False) as (conn, _):
        return [r[0] for r in conn.execute("SELECT job_id FROM event_send_jobs WHERE event_id = ?", (event_id,))]


def _sync_invites(event_id: int) -> tuple[list[dict[str, Any]], list[str]]:
    """Record as invited everyone the event's send jobs have sent to.

    Returns (job summaries, warnings). The bridge is the record of what was sent; this
    copies it into the list whenever an event is read, and is safe to repeat.
    """
    jobs: list[dict[str, Any]] = []
    warnings: list[str] = []
    sent: dict[str, str] = {}
    for job_id in _linked_jobs(event_id):
        r = whatsapp.get_send_job(job_id, recipient_status="sent")
        if not r.get("success"):
            warnings.append(f"Could not read send job {job_id}: {r.get('message')}")
            continue
        job = r.get("job") or {}
        jobs.append({k: job.get(k) for k in ("id", "label", "status", "counts", "dry_run", "pause_reason") if k in job})
        if job.get("dry_run"):
            continue
        for rec in job.get("recipients") or []:
            if rec.get("status") == "sent" and rec.get("jid"):
                sent.setdefault(rec["jid"], rec.get("sent_at") or _now())
    if sent:
        with _open(write=False) as (conn, _):
            listed = set(_person_rows(conn))
            rows = _event_rows(conn, event_id)
        todo = {j: at for j, at in sent.items() if j in listed and not (rows.get(j) and rows[j]["invited_at"])}
        if todo:
            with _open(write=True) as (conn, _):
                conn.executemany(
                    "INSERT INTO event_people (event_id, jid, invited_at) VALUES (?, ?, ?) "
                    "ON CONFLICT (event_id, jid) DO UPDATE SET invited_at = COALESCE(invited_at, excluded.invited_at)",
                    [(event_id, j, at) for j, at in todo.items()],
                )
    return jobs, warnings


def _group_sync_due(event: dict[str, Any]) -> bool:
    if not event["group_jid"]:
        return False
    if not event["group_synced_at"]:
        return True
    try:
        last = dt.datetime.fromisoformat(event["group_synced_at"])
    except ValueError:
        return True
    return (dt.datetime.now().astimezone() - last).total_seconds() > _GROUP_SYNC_MAX_AGE_S


def event_report(event_id: Any = None, sync: bool = True) -> dict[str, Any]:
    try:
        if event_id is None:
            with _open(write=False) as (conn, _):
                events = [dict(r) for r in conn.execute("SELECT id, name, date, group_jid FROM events ORDER BY date")]
                for event in events:
                    counts = dict.fromkeys(EVENT_STATUSES, 0)
                    for row in _event_rows(conn, event["id"]).values():
                        status = _event_status(row)
                        if status in counts:
                            counts[status] += 1
                    event["counts"] = counts
            return {"success": True, "events": events}
        eid = _event_id(event_id)
        with _open(write=False) as (conn, _):
            event = _load_event(conn, eid)
        warnings: list[str] = []
        jobs: list[dict[str, Any]] = []
        group_sync: dict[str, Any] | None = None
        if sync:
            jobs, warnings = _sync_invites(eid)
            if _group_sync_due(event):
                synced = sync_event_group(eid)
                if synced.get("success"):
                    group_sync = {
                        k: synced[k] for k in ("synced_at", "not_on_list_count", "newly_joined", "left_group")
                    }
                else:
                    warnings.append(synced["message"])
        with _open(write=False) as (conn, _):
            event = _load_event(conn, eid)
            people = _person_rows(conn)
            rows = _event_rows(conn, eid)
        by_status: dict[str, list[dict[str, Any]]] = {s: [] for s in EVENT_STATUSES}
        for jid, row in rows.items():
            status = _event_status(row)
            if status in by_status and jid in people:
                p = people[jid]
                entry = {k: p[k] for k in ("jid", "phone", "display_name", "flavour", "place")}
                entry.update({k: row[k] for k in ("invited_at", "joined_at", "attended_at", "declined_at") if row[k]})
                by_status[status].append(entry)
        for entries in by_status.values():
            entries.sort(key=lambda e: e["display_name"].casefold())
        result: dict[str, Any] = {
            "success": True,
            "event": event,
            "counts": {s: len(v) for s, v in by_status.items()},
            "ever_invited": sum(1 for r in rows.values() if r["invited_at"]),
            "people": by_status,
            "send_jobs": jobs,
        }
        if group_sync:
            result["group_sync"] = group_sync
        if warnings:
            result["warnings"] = warnings
        return result
    except PeopleError as e:
        return _failure(str(e))


# --- Send-job integration ----------------------------------------------------------------


def start_send_job(
    message_template: str,
    recipients: Any = None,
    people_filter: Any = None,
    event_id: Any = None,
    label: str | None = None,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Start a slow-send job for explicit recipients and/or people chosen by a filter.

    With event_id, everyone the job sends to is recorded as invited to that event.
    """
    try:
        explicit: list[dict[str, str]] = []
        if recipients is not None and recipients != []:
            explicit, error = whatsapp._normalize_send_job_recipients(recipients)
            if error:
                raise PeopleError(error)
        chosen: list[dict[str, str]] = []
        if people_filter is not None:
            f = _filter_from_dict(people_filter)
            if f["event_id"] is not None:
                _sync_invites(f["event_id"])
            with _open(write=False) as (conn, _):
                matched = _select(conn, f)
            if not matched:
                raise PeopleError("people_filter matched nobody on the list; check it with query_people")
            chosen = [{"recipient": p["jid"], "name": p["display_name"]} for p in matched]
        if not explicit and not chosen:
            raise PeopleError("Give recipients, a people_filter, or both")
        event = None
        if event_id is not None:
            eid = _event_id(event_id)
            with _open(write=False) as (conn, _):
                event = _load_event(conn, eid)
                listed = _person_rows(conn)
            missing = []
            for item in explicit:
                try:
                    jid = canonical_jid(item["recipient"])
                except PeopleError:
                    jid = None
                if jid not in listed:
                    missing.append(item["recipient"])
                else:
                    item.setdefault("name", listed[jid]["display_name"])
            if missing:
                raise PeopleError(
                    "With event_id every recipient must be on the people list (add_person first): " + ", ".join(missing)
                )
        merged = list(explicit)
        seen = set()
        for item in explicit:
            with contextlib.suppress(PeopleError):
                seen.add(canonical_jid(item["recipient"]))
        merged += [c for c in chosen if c["recipient"] not in seen]
        if not label and event:
            label = event["name"]
        result = whatsapp.start_send_job(merged, message_template, label=label, dry_run=dry_run)
        if result.get("success") and event is not None:
            job = result.get("job") or {}
            if job.get("dry_run"):
                result["event_note"] = "Dry run: nobody will be recorded as invited."
            elif job.get("id"):
                with _open(write=True) as (conn, notes):
                    conn.execute(
                        "INSERT OR IGNORE INTO event_send_jobs (event_id, job_id) VALUES (?, ?)",
                        (event["id"], job["id"]),
                    )
                result["event_id"] = event["id"]
                result["event_note"] = (
                    f"Everyone this job sends to is recorded as invited to {event['name']!r} "
                    "(event_report and query_people pick it up)."
                )
                if notes:
                    result["notes"] = notes
        if people_filter is not None:
            result["people_selected"] = len(chosen)
        return result
    except PeopleError as e:
        return _failure(str(e))


# --- Review page -------------------------------------------------------------------------

_FLAVOUR_COLOURS = {"cool": "#3b82c4", "warm": "#d98a1f", "hot": "#d0453a"}


def _download_avatar(url: str | None) -> str | None:
    """Fetch a WhatsApp CDN picture and return it as a data: URI, or None."""
    if not url:
        return None
    parsed = urlparse(url)
    host = (parsed.hostname or "").lower()
    if parsed.scheme != "https" or not (host == "whatsapp.net" or host.endswith(".whatsapp.net")):
        return None
    try:
        with requests.get(url, timeout=10, stream=True) as r:
            if r.status_code != 200:
                return None
            data = b""
            for chunk in r.iter_content(65536):
                data += chunk
                if len(data) > _AVATAR_MAX_BYTES:
                    return None
    except requests.RequestException:
        return None
    if data.startswith(b"\xff\xd8\xff"):
        mime = "image/jpeg"
    elif data.startswith(b"\x89PNG\r\n\x1a\n"):
        mime = "image/png"
    elif data[:4] == b"RIFF" and data[8:12] == b"WEBP":
        mime = "image/webp"
    else:
        return None
    return f"data:{mime};base64,{base64.b64encode(data).decode('ascii')}"


def _initials(name: str) -> str:
    parts = [p for p in name.split() if p[:1].isalnum()]
    return "".join(p[0] for p in parts[:2]).upper() or "?"


def _card(person: dict[str, Any], avatar: str | None) -> str:
    e = html.escape
    picture = (
        f'<img src="{e(avatar)}" alt="">'
        if avatar
        else f'<div class="initials">{e(_initials(person["display_name"]))}</div>'
    )
    phone = f"+{person['phone']}" if person.get("phone") else person["jid"]
    tags = "".join(f'<span class="tag">{e(t)}</span>' for t in person["tags"])
    status = f'<div class="status">{e(person["event_status"])}</div>' if person.get("event_status") else ""
    colour = _FLAVOUR_COLOURS[person["flavour"]]
    return (
        f'<article class="card"><div class="pic">{picture}</div><div class="info">'
        f'<h2>{e(person["display_name"])}</h2><div class="phone">{e(phone)}</div>'
        f'<div class="meta"><span class="flavour" style="--c:{colour}">{e(person["flavour"])}</span>'
        f'<span class="place">{e(person["place"] or "no place")}</span></div>'
        f'<div class="tags">{tags}</div>{status}</div></article>'
    )


_PAGE_CSS = """
:root { color-scheme: light dark; --bg:#f7f6f3; --fg:#1d1d1b; --muted:#6b6a66; --card:#fff; --line:#e4e2dc; }
@media (prefers-color-scheme: dark) { :root { --bg:#161615; --fg:#eceae4; --muted:#9a988f; --card:#20201e; --line:#33322e; } }
* { box-sizing: border-box; }
body { margin: 0; padding: 32px; background: var(--bg); color: var(--fg);
       font: 15px/1.4 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
header { max-width: 1200px; margin: 0 auto 24px; }
h1 { font-size: 22px; margin: 0 0 4px; }
.sub { color: var(--muted); font-size: 13px; }
.grid { max-width: 1200px; margin: 0 auto; display: grid; gap: 12px;
        grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); }
.card { display: flex; gap: 12px; padding: 12px; background: var(--card); border: 1px solid var(--line);
        border-radius: 10px; }
.pic img, .initials { width: 64px; height: 64px; border-radius: 50%; object-fit: cover; flex: none; }
.initials { display: grid; place-items: center; background: var(--line); color: var(--muted); font-weight: 600; }
.info { min-width: 0; }
h2 { font-size: 15px; margin: 0; overflow-wrap: anywhere; }
.phone { color: var(--muted); font-size: 13px; font-variant-numeric: tabular-nums; }
.meta { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 6px; font-size: 12px; }
.flavour { color: #fff; background: var(--c); padding: 1px 8px; border-radius: 99px; font-weight: 600; }
.place { color: var(--muted); }
.tags { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 6px; }
.tag { font-size: 11px; padding: 1px 6px; border: 1px solid var(--line); border-radius: 4px; color: var(--muted); }
.status { margin-top: 6px; font-size: 12px; font-weight: 600; }
footer { max-width: 1200px; margin: 24px auto 0; color: var(--muted); font-size: 12px; }
"""


def _describe_filter(f: dict[str, Any]) -> str:
    parts = []
    if f["min_flavour"]:
        parts.append(f"{FLAVOURS[f['min_flavour'] - 1]} or above")
    if f["places"]:
        parts.append("in " + ", ".join(sorted(f["places"])))
    if f["tags_include"]:
        parts.append("tagged " + " and ".join(sorted(f["tags_include"])))
    if f["tags_any"]:
        parts.append("tagged " + " or ".join(sorted(f["tags_any"])))
    if f["tags_exclude"]:
        parts.append("not tagged " + ", ".join(sorted(f["tags_exclude"])))
    if f["event_id"] is not None:
        statuses = ", ".join(sorted(f["event_statuses"])) or "any status"
        parts.append(f"event {f['event_id']} ({statuses})")
    return "; ".join(parts) or "everyone"


def render_review_page(
    min_flavour: str | None = None,
    places: Any = None,
    tags_include: Any = None,
    tags_any: Any = None,
    tags_exclude: Any = None,
    event_id: Any = None,
    event_statuses: Any = None,
    title: str | None = None,
    with_avatars: bool = True,
) -> dict[str, Any]:
    """Write a local HTML contact sheet of the matching people and return its path."""
    try:
        f = _normalize_filter(min_flavour, places, tags_include, tags_any, tags_exclude, event_id, event_statuses)
        folder = review_dir()
        _refuse_cloud_synced(folder, "WHATSAPP_PEOPLE_REVIEW_DIR")
        if f["event_id"] is not None:
            _sync_invites(f["event_id"])
        with _open(write=False) as (conn, _):
            people = _select(conn, f)
        avatars: dict[str, str | None] = {}
        if with_avatars:
            for i, person in enumerate(people):
                if i:
                    time.sleep(_AVATAR_PAUSE_S)
                pic = whatsapp.get_profile_picture(person["jid"], preview=True)
                avatars[person["jid"]] = _download_avatar(pic.get("url") if pic.get("success") else None)
        heading = _clean_text(title, "title", 120) or "People review"
        generated = dt.datetime.now().astimezone().strftime("%Y-%m-%d %H:%M")
        cards = "\n".join(_card(p, avatars.get(p["jid"])) for p in people)
        page = (
            '<!doctype html><html lang="en"><head><meta charset="utf-8">'
            '<meta name="viewport" content="width=device-width, initial-scale=1">'
            '<meta name="robots" content="noindex">'
            f"<title>{html.escape(heading)}</title><style>{_PAGE_CSS}</style></head><body>"
            f'<header><h1>{html.escape(heading)}</h1><div class="sub">{len(people)} people · '
            f"{html.escape(_describe_filter(f))} · generated {generated}</div></header>"
            f'<main class="grid">{cards}</main>'
            "<footer>Private: this page holds personal details of other people. Delete it when you are done.</footer>"
            "</body></html>"
        )
        new_folder = not os.path.isdir(folder)
        os.makedirs(folder, mode=0o700, exist_ok=True)
        if new_folder:
            _exclude_from_backups(folder)
        path = os.path.join(folder, f"people-review-{dt.datetime.now().strftime('%Y%m%d-%H%M%S')}.html")
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(page)
        return {
            "success": True,
            "message": f"Wrote a contact sheet of {len(people)} people. It is private: delete it when done.",
            "path": path,
            "count": len(people),
            "avatars_included": sum(1 for v in avatars.values() if v),
        }
    except PeopleError as e:
        return _failure(str(e))
