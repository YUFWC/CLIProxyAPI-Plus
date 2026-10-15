"""Offline regression tests for lobsterai_server_checkin; never call the live service."""
import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from lobsterai_server_checkin import (
    DEFAULT_PLACEMENT,
    REFRESH_PATH,
    _run_one,
    build_headers,
    load_session,
    main,
    refresh_credential,
    save_session,
)

BACKEND = "https://lobsterai-server.youdao.com"

CREDENTIAL = {
    "type": "lobsterai-server",
    "access_token": "access-1",
    "refresh_token": "refresh-1",
    "backend_url": BACKEND,
    "base_url": f"{BACKEND}/api/proxy/v1",
    "client_version": "2026.10.13",
}

SLOT_AVAILABLE = {"status": 200, "payload": {"code": 0, "data": {
    "slotState": "available",
    "activity": {"activityCode": "daily-check-in-evergreen-prod-20260814", "configRevision": 1,
                 "activityType": "daily_check_in", "cardTitle": "每日积分礼"}}}}
SLOT_EMPTY = {"status": 200, "payload": {"code": 0, "data": {"slotState": "empty", "activity": None}}}
CTX_TODAY_OPEN = {"status": 200, "payload": {"code": 0, "data": {
    "state": {"claimedToday": False, "completed": False, "rewardCredits": 100, "claimedDays": 0},
    "actions": ["check_in"]}}}
CTX_TODAY_DONE = {"status": 200, "payload": {"code": 0, "data": {
    "state": {"claimedToday": True, "completed": False, "claimedDays": 1, "claimedCredits": 100.0},
    "actions": []}}}
CLAIM_OK = {"status": 200, "payload": {"code": 0, "data": {
    "replayed": False, "result": {"creditsGranted": 100, "periodKey": "2026-10-15"}}}}
CLAIM_ALREADY = {"status": 200, "payload": {"code": 51104, "message": "已领取"}}
REFRESH_OK = {"status": 200, "payload": {"code": 0, "data": {
    "accessToken": "access-new", "refreshToken": "refresh-new", "expiresIn": 3600}}}
REFRESH_ROTATIONLESS = {"status": 200, "payload": {"code": 0, "data": {"accessToken": "access-new"}}}


def write_credential(directory: str, name: str, content: dict) -> Path:
    path = Path(directory) / name
    path.write_text(json.dumps(content), encoding="utf-8")
    return path


class LoadSessionTests(unittest.TestCase):
    def test_credential_file_supplies_token_and_backend(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_credential(directory, "lobsterai-server-1.json", CREDENTIAL)
            with contextlib.redirect_stdout(io.StringIO()):
                session = load_session(path, None, None)
        self.assertEqual(session["token"], "access-1")
        self.assertEqual(session["backend"], BACKEND)
        self.assertEqual(session["client_version"], "2026.10.13")
        self.assertEqual(session["refresh_token"], "refresh-1")

    def test_backend_is_derived_from_base_url_when_absent(self):
        credential = dict(CREDENTIAL)
        credential.pop("backend_url")
        with tempfile.TemporaryDirectory() as directory:
            path = write_credential(directory, "lobsterai-server-1.json", credential)
            with contextlib.redirect_stdout(io.StringIO()):
                session = load_session(path, None, None)
        self.assertEqual(session["backend"], BACKEND)

    def test_manual_token_needs_no_file(self):
        session = load_session(None, "tok", BACKEND)
        self.assertEqual(session["token"], "tok")
        self.assertIsNone(session["_path"])

    def test_missing_access_token_is_an_error(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_credential(directory, "x.json", {"type": "lobsterai-server"})
            with self.assertRaises(SystemExit):
                load_session(path, None, None)


class HeaderTests(unittest.TestCase):
    def test_client_identification_headers_are_sent(self):
        headers = build_headers({"token": "t", "client_version": "2026.10.13"})
        self.assertEqual(headers["Authorization"], "Bearer t")
        self.assertEqual(headers["X-LobsterAI-Client-Version"], "2026.10.13")
        # The gateway gates some request shapes on this capability list.
        self.assertIn("kimi-k3-agentic-v1", headers["X-LobsterAI-Client-Capabilities"])


class RefreshTests(unittest.TestCase):
    def test_request_shape_matches_auth_refresh(self):
        seen = {}

        def fake(url, headers, body, timeout, method="POST"):
            seen["url"] = url
            seen["body"] = body
            return REFRESH_OK

        with patch("lobsterai_server_checkin._http_json", fake):
            result = refresh_credential({"backend": BACKEND, "refresh_token": "refresh-1",
                                         "client_version": "2026.10.13"}, 5)
        self.assertEqual(seen["url"], f"{BACKEND}{REFRESH_PATH}")
        self.assertEqual(seen["body"]["refreshToken"], "refresh-1")
        # The upstream expects the attribution payload on login/refresh requests.
        self.assertIn("firstKeyfrom", seen["body"])
        self.assertEqual(result["token"], "access-new")

    def test_missing_refresh_token_is_an_error(self):
        with self.assertRaises(RuntimeError):
            refresh_credential({"backend": BACKEND, "refresh_token": ""}, 5)

    def test_rotated_token_kept_when_response_omits_it(self):
        with patch("lobsterai_server_checkin._http_json", lambda *a, **k: REFRESH_ROTATIONLESS):
            result = refresh_credential({"backend": BACKEND, "refresh_token": "refresh-1"}, 5)
        self.assertEqual(result["refresh_token"], "refresh-1")

    def test_rejected_refresh_reports_code(self):
        rejected = {"status": 200, "payload": {"code": 40100, "message": "登录已过期"}}
        with patch("lobsterai_server_checkin._http_json", lambda *a, **k: rejected):
            with self.assertRaises(RuntimeError):
                refresh_credential({"backend": BACKEND, "refresh_token": "r"}, 5)


class SaveSessionTests(unittest.TestCase):
    def test_rotation_is_written_in_place_and_keeps_other_fields(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_credential(directory, "lobsterai-server-1.json", CREDENTIAL)
            session = {"_path": str(path)}
            save_session(session, {"token": "access-new", "refresh_token": "refresh-new",
                                   "expires_in": 3600, "expired": "2030-01-01T00:00:00Z"})
            saved = json.loads(path.read_text(encoding="utf-8"))
        self.assertEqual(saved["access_token"], "access-new")
        self.assertEqual(saved["refresh_token"], "refresh-new")
        self.assertEqual(saved["expired"], "2030-01-01T00:00:00Z")
        # Foreign fields must survive the write-back.
        self.assertEqual(saved["backend_url"], BACKEND)
        self.assertEqual(saved["type"], "lobsterai-server")

    def test_manual_token_mode_cannot_write_back(self):
        with self.assertRaises(RuntimeError):
            save_session({"_path": None}, {"token": "t", "refresh_token": "", "expires_in": 0, "expired": ""})


class RunOneTests(unittest.TestCase):
    def _args(self, **overrides):
        base = {"refresh_token": False, "action": "checkin", "timeout": 5, "include_startup": False}
        base.update(overrides)
        return SimpleNamespace(**base)

    def _session(self):
        return {"token": "t", "backend": BACKEND, "client_version": "2026.10.13",
                "refresh_token": "", "_path": None}

    def test_check_in_executes_and_reports_success(self):
        calls = []

        def fake(url, headers, body, timeout, method="POST"):
            calls.append((method, url, body))
            if "slot" in url:
                return SLOT_AVAILABLE
            if "context" in url:
                return CTX_TODAY_OPEN
            return CLAIM_OK

        with patch("lobsterai_server_checkin._http_json", fake):
            with contextlib.redirect_stdout(io.StringIO()):
                rc = _run_one(self._args(), self._session())
        self.assertEqual(rc, 0)
        action = [c for c in calls if "actions/check_in" in c[1]]
        self.assertEqual(len(action), 1)
        method, url, body = action[0]
        self.assertEqual(method, "POST")
        self.assertEqual(body["configRevision"], 1)
        self.assertEqual(body["payload"], {})
        # The key must satisfy the client's ^[A-Za-z0-9._:-]{1,64}$ rule.
        self.assertRegex(body["idempotencyKey"], r"^[A-Za-z0-9._:-]{1,64}$")

    def test_already_claimed_today_does_not_post(self):
        calls = []

        def fake(url, headers, body, timeout, method="POST"):
            calls.append(url)
            if "slot" in url:
                return SLOT_AVAILABLE
            return CTX_TODAY_DONE

        with patch("lobsterai_server_checkin._http_json", fake):
            with contextlib.redirect_stdout(io.StringIO()):
                rc = _run_one(self._args(), self._session())
        self.assertEqual(rc, 0)
        self.assertFalse(any("actions/" in u for u in calls), "must not claim twice in a day")

    def test_status_mode_never_claims(self):
        calls = []

        def fake(url, headers, body, timeout, method="POST"):
            calls.append(url)
            if "slot" in url:
                return SLOT_AVAILABLE
            return CTX_TODAY_OPEN

        with patch("lobsterai_server_checkin._http_json", fake):
            with contextlib.redirect_stdout(io.StringIO()):
                rc = _run_one(self._args(action="status"), self._session())
        self.assertEqual(rc, 0)
        self.assertFalse(any("actions/" in u for u in calls))

    def test_empty_slot_is_a_no_op(self):
        with patch("lobsterai_server_checkin._http_json",
                   lambda url, headers, body, timeout, method="POST": SLOT_EMPTY):
            with contextlib.redirect_stdout(io.StringIO()):
                rc = _run_one(self._args(), self._session())
        self.assertEqual(rc, 0)

    def test_51104_is_treated_as_already_done(self):
        def fake(url, headers, body, timeout, method="POST"):
            if "slot" in url:
                return SLOT_AVAILABLE
            if "context" in url:
                return CTX_TODAY_OPEN
            return CLAIM_ALREADY

        with patch("lobsterai_server_checkin._http_json", fake):
            with contextlib.redirect_stdout(io.StringIO()):
                rc = _run_one(self._args(), self._session())
        self.assertEqual(rc, 0)

    def test_refresh_failure_skips_the_claim(self):
        calls = []

        def fake(url, headers, body, timeout, method="POST"):
            calls.append(url)
            return {"status": 500, "payload": {}}

        with patch("lobsterai_server_checkin._http_json", fake):
            with contextlib.redirect_stdout(io.StringIO()):
                rc = _run_one(self._args(refresh_token=True), self._session())
        self.assertEqual(rc, 1)
        self.assertFalse(any("slot" in u or "actions/" in u for u in calls),
                         "a failed refresh must not proceed to status/claim")

    def test_slot_options_advertise_a_real_platform(self):
        # platform=cli makes the backend hide the activity, so the query must
        # carry win32/darwin/linux.
        seen = {}

        def fake(url, headers, body, timeout, method="POST"):
            seen["url"] = url
            return SLOT_EMPTY

        with patch("lobsterai_server_checkin._http_json", fake):
            with contextlib.redirect_stdout(io.StringIO()):
                _run_one(self._args(), self._session())
        self.assertIn(f"placement={DEFAULT_PLACEMENT}", seen["url"])
        self.assertIn("containerApiVersion=2", seen["url"])
        self.assertNotIn("platform=cli", seen["url"])


class MainTests(unittest.TestCase):
    def test_prefix_requires_auth_dir(self):
        with self.assertRaises(SystemExit):
            main(["--prefix", "lobsterai"])

    def test_refresh_token_conflicts_with_manual_token(self):
        with self.assertRaises(SystemExit):
            main(["--refresh-token", "--token", "t"])

    def test_auth_dir_batches_every_credential(self):
        with tempfile.TemporaryDirectory() as directory:
            write_credential(directory, "lobsterai-server-a.json", CREDENTIAL)
            write_credential(directory, "lobsterai-server-b.json", CREDENTIAL)
            write_credential(directory, "other.json", CREDENTIAL)
            with patch("lobsterai_server_checkin._http_json",
                       lambda url, headers, body, timeout, method="POST": SLOT_EMPTY):
                with contextlib.redirect_stdout(io.StringIO()):
                    rc = main(["status", "--auth-dir", directory, "--prefix", "lobsterai-server"])
        self.assertEqual(rc, 0)


if __name__ == "__main__":
    unittest.main()
