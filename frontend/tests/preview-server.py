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
        fixtures = {"/api/ui/activation": {"challenge": True}, "/api/settings/visibility/unlock": {"verified": True}}
        if self.authenticated and self.path in fixtures:
            return self.api(200, {"data": fixtures[self.path]})
        self.api(404, {"error": {"code": "PREVIEW_ONLY"}})

    def do_DELETE(self):
        if self.path == "/api/session":
            return self.api(200, {"data": None})
        self.api(404, {"error": {"code": "PREVIEW_ONLY"}})

    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(ROOT), **kwargs)

    def do_GET(self):
        path = urlsplit(self.path).path
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
    args = parser.parse_args()
    Preview.authenticated = args.authenticated
    Preview.modules = args.modules
    if args.modules:
        for n in range(1, 9):
            esim = {"eid": "89049032001001234500012345678901", "pending": 1, "profiles": []}
            Preview.devices.append({
                "id": f"module-{n:02d}", "name": f"模块 {n:02d}", "label": f"模块 {n:02d}",
                "labelCustom": False, "managed": True, "kind": "usb", "number": "", "signal": "cellular",
                "status": "offline" if n == 8 else "online", "capabilities": {"esim": n != 8},
                "hardware": {"model": "EC20", "imei": "123456789012345", "iccid": "89123456789012345678", "simState": "READY", "registration": "home", "operator": "测试运营商", "technology": "LTE", "rssi": -69, "esim": esim},
                "sims": [{"id": "fixture-main", "iccid": "89123456789012345678", "label": "主号", "number": "", "enabled": True, "esim": True, "canDisable": True, "canDelete": True}, {"id": "fixture-spare", "iccid": "89123456789012345679", "label": "备用", "number": "", "enabled": False, "esim": True, "canDisable": True, "canDelete": True}],
            })
    ThreadingHTTPServer(("127.0.0.1", args.port), Preview).serve_forever()
