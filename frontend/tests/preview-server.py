"""隔离外观检查，不读取或写入用户存储。"""
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit
import argparse
import json

ROOT = Path(__file__).resolve().parent.parent


class Preview(SimpleHTTPRequestHandler):
    authenticated = False
    modules = False
    devices = []
    apn_profiles = {}
    messages = []
    contacts = []

    def apn_path(self):
        parts = urlsplit(self.path).path.strip("/").split("/")
        return parts if self.authenticated and self.modules and len(parts) >= 6 and parts[:2] == ["api", "modules"] and parts[3] == "lines" and parts[5] == "apns" else None

    def do_PUT(self):
        if self.authenticated and self.path == "/api/messages/contacts":
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
            body["revision"] = len(self.contacts) + 1
            self.contacts.append(body)
            return self.api(200, {"data": body})
        parts = self.apn_path()
        if not parts or len(parts) != 7:
            return self.api(404, {"error": {"code": "PREVIEW_ONLY"}})
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        key = (parts[2], parts[4], parts[6])
        profile = {k: body.get(k, "") for k in ("apn", "protocol", "auth", "username")}
        profile.update(id=parts[6], hasPassword=bool(body.get("password")) or (body.get("preservePassword") and self.apn_profiles.get(key, {}).get("hasPassword", False)))
        self.apn_profiles[key] = profile
        return self.api(200, {"data": profile})

    def do_PATCH(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        item = next((v for v in self.devices if self.path == "/api/modules/" + v["id"]), None)
        if not self.modules or not item:
            return self.api(404, {"error": {"code": "PREVIEW_ONLY"}})
        label = body.get("label", "").strip() or item["name"]
        if any(v["id"] != item["id"] and v["label"] == label for v in self.devices):
            return self.api(409, {"error": {"code": "LABEL_EXISTS"}})
        item["label"], item["labelCustom"] = label, True
        return self.api(200, {"data": item})

    def api(self, status, value):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        parts = self.apn_path()
        if parts and len(parts) == 8 and parts[-1] == "apply":
            return self.api(200, {"data": {"applied": True, "dataEnabled": False}})
        fixtures = {"/api/ui/activation": {"challenge": True}, "/api/settings/visibility/unlock": {"verified": True}}
        if self.authenticated and self.path in fixtures:
            return self.api(200, {"data": fixtures[self.path]})
        self.api(404, {"error": {"code": "PREVIEW_ONLY"}})

    def do_DELETE(self):
        parts = self.apn_path()
        if parts and len(parts) == 7:
            self.apn_profiles.pop((parts[2], parts[4], parts[6]), None)
            return self.api(200, {"data": None})
        if self.path == "/api/session":
            return self.api(200, {"data": None})
        self.api(404, {"error": {"code": "PREVIEW_ONLY"}})

    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(ROOT), **kwargs)

    def do_GET(self):
        path = urlsplit(self.path).path
        parts = self.apn_path()
        if self.authenticated and path == "/api/messages":
            return self.api(200, {"data": {"items": self.messages, "cursor": len(self.messages), "more": False, "contacts": self.contacts}})
        if parts and len(parts) == 6:
            profiles = [p for (module, line, _), p in self.apn_profiles.items() if (module, line) == (parts[2], parts[4])]
            current = [{"cid": 1, "apn": "", "protocol": "IPV4V6"}, {"cid": 2, "apn": "ims", "protocol": "IPV4V6"}, {"cid": 3, "apn": "SOS", "protocol": "IPV4V6"}]
            return self.api(200, {"data": {"profiles": profiles, "current": current, "issue": ""}})
        if self.authenticated and path == "/api/modules":
            return self.api(200, {"data": {"items": self.devices, "discoveryIssue": ""}})
        if path == "/api/session":
            if self.authenticated:
                return self.api(200, {"data": {"user": {"id": "layout-preview"}, "csrfToken": "preview-only"}})
            self.send_response(401)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":{"code":"UNAUTHENTICATED"}}')
            return
        if self.authenticated and path == "/api/settings/visibility":
            return self.api(200, {"data": {"features": {}}})
        if self.authenticated and path == "/api/settings/visibility/access":
            return self.api(200, {"data": {"verified": True}})
        if path == "/__preview-state.js":
            body = b'''UI.read = (key, fallback) => key === "rykvo-voice-v1"
              ? {theme: new URLSearchParams(location.search).get("theme") === "dark" ? "dark" : "light", motion: true}
              : fallback;
              UI.write = () => true;'''
            kind = "text/javascript"
        elif path == "/login.js":
            code = (ROOT / "login.js").read_text(encoding="utf-8")
            start = code.index("  try {\n    const preferences")
            code = code[:start] + '  start();\n})();'
            body, kind = code.encode("utf-8"), "text/javascript"
        elif path in ("/", "/index.html") and not self.authenticated:
            html = (ROOT / "login.html").read_text(encoding="utf-8")
            if "theme=dark" in self.path:
                html = html.replace("<body>", '<body class="theme-dark">')
            body, kind = html.encode("utf-8"), "text/html"
        elif path in ("/", "/index.html"):
            html = (ROOT / "index.html").read_text(encoding="utf-8")
            marker = html.index("</script>", html.index('src="shared.js')) + 9
            html = html[:marker] + '<script src="/__preview-state.js" defer></script>' + html[marker:]
            body, kind = html.encode("utf-8"), "text/html"
        else:
            return super().do_GET()
        self.send_response(200)
        self.send_header("Content-Type", kind + "; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=5174)
    parser.add_argument("--authenticated", action="store_true", help="Isolated layout fixture, not real authentication")
    parser.add_argument("--modules", action="store_true", help="Isolated EC20/eUICC layout fixture")
    parser.add_argument("--messages", action="store_true", help="Isolated SMS conversation fixture")
    args = parser.parse_args()
    Preview.authenticated = args.authenticated
    Preview.modules = args.modules
    if args.messages:
        import time
        peers = ["+13322500550", "13322500550", "3322500550", "+447598999919", "07598999919", "7598999919", "+85262717066", "852 62717066", "62717066", "#DIYsim", "54623"]
        for i, peer in enumerate(peers):
            Preview.messages.append({"id": f"preview-{i}", "number": peer, "senderId": "module-01", "lineId": "fixture-main", "mine": i % 3 != 0, "text": "这是一条预览信息。" if i % 3 == 0 else "收到，稍后联系。", "image": "", "kind": "sms", "state": ["received", "delivered", "sending"][i % 3], "at": int(time.time()*1000)-(len(peers)-i)*60000, "revision": i+1})
        for state in ["received", "received", "received", "accepted", "delivered", "failed", "unknown", "sending"]:
            i = len(Preview.messages)
            Preview.messages.append({"id": f"preview-{i}", "number": "+13322500550", "senderId": "module-01", "lineId": "fixture-main", "mine": state != "received", "text": "测试" if state == "received" else "收到，稍后联系。", "image": "", "kind": "sms", "state": state, "at": int(time.time()*1000)+i*1000, "revision": i+1})
    if args.modules:
        for n in range(1, 9):
            esim = {"eid": "89049032001001234500012345678901", "pending": 1, "profiles": []}
            Preview.devices.append({
                "id": f"module-{n:02d}", "name": f"模块 {n:02d}", "label": f"模块 {n:02d}",
                "labelCustom": False, "managed": True, "kind": "usb", "number": "", "signal": "cellular",
                "status": "offline" if n == 8 else "online", "capabilities": {"esim": n != 8, "restart": n != 8},
                "hardware": {"model": "EC20", "imei": "123456789012345", "iccid": "89123456789012345678", "simState": "READY", "registration": "home", "operator": "测试运营商", "technology": "LTE", "rssi": -69, "esim": esim},
                "sims": [{"id": "fixture-main", "iccid": "89123456789012345678", "label": "主号", "number": "", "enabled": True, "esim": True, "canDisable": True, "canDelete": True}, {"id": "fixture-spare", "iccid": "89123456789012345679", "label": "备用", "number": "", "enabled": False, "esim": True, "canDisable": True, "canDelete": True}],
            })
    ThreadingHTTPServer(("127.0.0.1", args.port), Preview).serve_forever()
