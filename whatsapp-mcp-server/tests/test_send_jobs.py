"""Tests for the slow-send job client functions and MCP tools (fork).

Every request goes to a fake ``requests.post``/``requests.get``; nothing reaches the
bridge or WhatsApp.
"""

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


class Calls(list):
    """Recorded requests, plus the response the fake bridge should give."""

    def __init__(self):
        super().__init__()
        self.state = {"status": 200, "payload": {"success": True}}


@pytest.fixture
def posts(monkeypatch):
    calls = Calls()
    state = calls.state

    def fake_post(url, json, headers=None, **kwargs):
        calls.append({"url": url, "json": json, "headers": headers})
        return DummyResponse(status_code=state["status"], payload=state["payload"])

    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "tok")
    monkeypatch.setattr(whatsapp.requests, "post", fake_post)
    return calls


@pytest.fixture
def gets(monkeypatch):
    calls = []

    def fake_get(url, params=None, headers=None, **kwargs):
        calls.append({"url": url, "params": params, "headers": headers, "kwargs": kwargs})
        return DummyResponse(payload={"success": True, "job": {"id": 7}})

    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "tok")
    monkeypatch.setattr(whatsapp.requests, "get", fake_get)
    return calls


BASE = whatsapp.WHATSAPP_API_BASE_URL


# --- start_send_job ---------------------------------------------------------


def test_start_send_job_posts_normalised_recipients(posts):
    whatsapp.start_send_job(
        [
            {"recipient": " 447700900001 ", "name": " Alice "},
            {"jid": "447700900002@s.whatsapp.net", "name": "Bob"},
            {"phone": "+44 7700 900003"},
            " 184125298348272@lid ",
        ],
        "Hi {name}, party on Saturday!",
        label="  Birthday  ",
    )

    assert posts[0]["url"] == f"{BASE}/send-jobs"
    assert posts[0]["headers"] == {"Authorization": "Bearer tok"}
    assert posts[0]["json"] == {
        "recipients": [
            {"recipient": "447700900001", "name": "Alice"},
            {"recipient": "447700900002@s.whatsapp.net", "name": "Bob"},
            {"recipient": "+44 7700 900003"},
            {"recipient": "184125298348272@lid"},
        ],
        "template": "Hi {name}, party on Saturday!",
        "label": "Birthday",
    }


def test_start_send_job_sends_dry_run_only_when_asked(posts):
    whatsapp.start_send_job(["447700900001"], "Hi")
    whatsapp.start_send_job(["447700900001"], "Hi", dry_run=True)

    assert "dry_run" not in posts[0]["json"]
    assert posts[1]["json"]["dry_run"] is True


def test_start_send_job_accepts_a_single_recipient_object(posts):
    whatsapp.start_send_job({"recipient": "447700900001", "name": "Al"}, "Hi {name}")

    assert posts[0]["json"]["recipients"] == [{"recipient": "447700900001", "name": "Al"}]


@pytest.mark.parametrize(
    ("recipients", "template", "expected"),
    [
        (None, "Hi", "At least one recipient"),
        ([], "Hi", "At least one recipient"),
        ([{"name": "Alice"}], "Hi", "Recipient 1 has no 'recipient'"),
        (["447700900001", "  "], "Hi", "Recipient 2 has no 'recipient'"),
        ([42], "Hi", "Recipient 1 must be"),
        (["447700900001"], "   ", "message_template"),
    ],
)
def test_start_send_job_validates_before_calling_bridge(posts, recipients, template, expected):
    result = whatsapp.start_send_job(recipients, template)

    assert result["success"] is False
    assert expected in result["message"]
    assert posts == []


def test_start_send_job_surfaces_bridge_rejection(posts):
    posts.state["status"] = 400
    posts.state["payload"] = {
        "success": False,
        "message": "job not created: 101 unique recipients is more than the per-job ceiling of 100",
        "errors": ["101 unique recipients is more than the per-job ceiling of 100"],
    }

    result = whatsapp.start_send_job(["447700900001"], "Hi")

    assert result["success"] is False
    assert "ceiling of 100" in result["message"]
    assert result["errors"] == ["101 unique recipients is more than the per-job ceiling of 100"]


def test_start_send_job_surfaces_overlap_conflict(posts):
    posts.state["status"] = 409
    posts.state["payload"] = {"success": False, "message": "job not created: ... Resume that job instead"}

    result = whatsapp.start_send_job(["447700900001"], "Hi")

    assert result["success"] is False
    assert "Resume that job" in result["message"]


def test_start_send_job_reports_unreachable_bridge(monkeypatch):
    def boom(*args, **kwargs):
        raise whatsapp.requests.ConnectionError("refused")

    monkeypatch.setattr(whatsapp.requests, "post", boom)

    result = whatsapp.start_send_job(["447700900001"], "Hi")

    assert result["success"] is False
    assert "Request error" in result["message"]


# --- reading jobs -----------------------------------------------------------


def test_get_send_job_hits_job_url_with_optional_filter(gets):
    whatsapp.get_send_job(7)
    whatsapp.get_send_job("7", recipient_status=" Failed ")

    assert gets[0]["url"] == f"{BASE}/send-jobs/7"
    assert gets[0]["params"] == {}
    assert gets[0]["headers"] == {"Authorization": "Bearer tok"}
    assert gets[1]["params"] == {"recipient_status": "failed"}
    assert gets[0]["kwargs"]["timeout"] == 15


@pytest.mark.parametrize("job_id", [0, -1, "abc", None, True, 1.5])
def test_get_send_job_rejects_bad_ids(gets, job_id):
    result = whatsapp.get_send_job(job_id)

    assert result["success"] is False
    assert "job_id" in result["message"]
    assert gets == []


def test_get_send_job_rejects_unknown_recipient_status(gets):
    result = whatsapp.get_send_job(7, recipient_status="delivered")

    assert result["success"] is False
    assert gets == []


def test_list_send_jobs_params(gets):
    whatsapp.list_send_jobs()
    whatsapp.list_send_jobs(status="Paused", limit=500)
    whatsapp.list_send_jobs(status="queued", limit=0)

    assert gets[0]["url"] == f"{BASE}/send-jobs"
    assert gets[0]["params"] == {"limit": "20"}
    assert gets[1]["params"] == {"status": "paused", "limit": "200"}
    assert gets[2]["params"] == {"status": "running", "limit": "1"}


def test_list_send_jobs_rejects_unknown_status(gets):
    result = whatsapp.list_send_jobs(status="done")

    assert result["success"] is False
    assert gets == []


# --- pause / resume / cancel ------------------------------------------------


def test_job_actions_post_to_their_urls(posts):
    whatsapp.pause_send_job(3)
    whatsapp.resume_send_job(3)
    whatsapp.resume_send_job("3", retry_failed=True)
    whatsapp.cancel_send_job(3)

    assert [(c["url"], c["json"]) for c in posts] == [
        (f"{BASE}/send-jobs/3/pause", {}),
        (f"{BASE}/send-jobs/3/resume", {"retry_failed": False}),
        (f"{BASE}/send-jobs/3/resume", {"retry_failed": True}),
        (f"{BASE}/send-jobs/3/cancel", {}),
    ]
    assert all(c["headers"] == {"Authorization": "Bearer tok"} for c in posts)


@pytest.mark.parametrize("func", ["pause_send_job", "resume_send_job", "cancel_send_job"])
def test_job_actions_reject_bad_ids(posts, func):
    result = getattr(whatsapp, func)("x")

    assert result["success"] is False
    assert posts == []


# --- MCP tools --------------------------------------------------------------


def test_mcp_tools_forward_arguments(monkeypatch):
    seen = []
    for name in (
        "get_send_job",
        "list_send_jobs",
        "pause_send_job",
        "resume_send_job",
        "cancel_send_job",
    ):
        monkeypatch.setattr(
            mcp_main,
            f"whatsapp_{name}",
            lambda *a, _name=name, **kw: seen.append((_name, a, kw)) or {"success": True},
        )
    monkeypatch.setattr(
        mcp_main, "people_start_send_job", lambda *a, **kw: seen.append(("start_send_job", a, kw)) or {"success": True}
    )

    mcp_main.start_send_job("Hi {name}", [{"recipient": "1", "name": "A"}], label="x", dry_run=True)
    mcp_main.get_send_job(4, recipient_status="sent")
    mcp_main.list_send_jobs(status="paused", limit=5)
    mcp_main.pause_send_job(4)
    mcp_main.resume_send_job(4, retry_failed=True)
    mcp_main.cancel_send_job(4)

    assert seen == [
        (
            "start_send_job",
            ("Hi {name}",),
            {
                "recipients": [{"recipient": "1", "name": "A"}],
                "people_filter": None,
                "event_id": None,
                "label": "x",
                "dry_run": True,
            },
        ),
        ("get_send_job", (4,), {"recipient_status": "sent"}),
        ("list_send_jobs", (), {"status": "paused", "limit": 5}),
        ("pause_send_job", (4,), {}),
        ("resume_send_job", (4,), {"retry_failed": True}),
        ("cancel_send_job", (4,), {}),
    ]


async def test_send_job_tools_are_registered_with_clear_schemas():
    tools = {t.name: t for t in await mcp_main.mcp.list_tools()}

    def text(name):  # descriptions wrap lines; compare with single spaces
        return " ".join(tools[name].description.split())

    start = tools["start_send_job"]
    assert set(start.inputSchema["properties"]) == {
        "recipients",
        "message_template",
        "people_filter",
        "event_id",
        "label",
        "dry_run",
    }
    assert start.inputSchema["required"] == ["message_template"]
    for phrase in ("45-90 second", "{name}", "dry_run", "at most once", "100 unique", "pauses itself", "queued"):
        assert phrase in text("start_send_job")
    assert tools["resume_send_job"].inputSchema["required"] == ["job_id"]
    assert "retry_failed" in text("resume_send_job")
    assert "cannot be resumed" in text("cancel_send_job")
    for name in ("get_send_job", "list_send_jobs", "pause_send_job"):
        assert name in tools


async def test_start_send_job_tool_returns_bridge_json(posts):
    posts.state["payload"] = {"success": True, "message": "Job 1 created", "job": {"id": 1, "status": "running"}}

    result = await mcp_main.mcp.call_tool(
        "start_send_job",
        {"recipients": [{"recipient": "447700900001", "name": "Alice"}], "message_template": "Hi {name}"},
    )

    content = result[0] if isinstance(result, tuple) else result
    assert json.loads(content[0].text)["job"]["id"] == 1
    assert posts[0]["json"]["recipients"] == [{"recipient": "447700900001", "name": "Alice"}]
