#!/usr/bin/env python3
"""只用于本地视觉核对：把 frontend/dist 当静态站点，并把 Wails 的绑定调用
喂成一组造出来的数据（其中一个应可更新），这样能看清界面在「有更新」时的样子。
不参与构建，也不进二进制。"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "frontend", "dist")

def status(app_id, name, accent, tagline, tagline_en, installed, version, latest, connected,
           transport="tcp", port=0, control="TCP 127.0.0.1:7990", path="", pid=0):
    return {
        "app": {
            "id": app_id, "name": name, "tagline": tagline, "taglineEn": tagline_en,
            "repo": "storytold/" + app_id, "accent": accent, "fallback": accent,
            "transport": transport, "defaultPort": port, "controlFlag": "--control",
        },
        "installed": installed, "path": path,
        "endpoint": {"network": "tcp" if transport == "tcp" else "unix",
                     "address": control.split()[-1] if connected else ""},
        "connected": connected, "pid": pid, "version": version, "control": control,
        "installedVersion": version, "versionSource": "bundle" if installed else "",
        "latestVersion": latest, "latestUrl": "https://github.com/storytold/%s/releases" % app_id,
        "latestNotes": "", "updateAvailable": bool(installed and latest and version and version != latest),
    }

SUITE = [
    status("photocraft", "PhotoCraft", "#2f7bf5", "图像编辑", "Image editing", False, "", "0.16.0", False),
    status("vectorcraft", "VectorCraft", "#e8573f", "矢量插画", "Vector illustration", False, "", "0.17.0", False),
    status("pdfcraft", "PdfCraft", "#12a58a", "PDF 阅读、整理与保护", "PDF reading, organizing, protecting",
           True, "0.14.0", "0.14.0", False, transport="unix", control="Unix socket"),
    status("deckcraft", "DeckCraft", "#f07a1e", "演示文稿与放映", "Presentations and slideshows",
           True, "0.11.1", "0.12.0", True, port=7990, control="TCP 127.0.0.1:7990",
           path="/Applications/DeckCraft.app/Contents/MacOS/DeckCraft", pid=18100),
    status("cadcraft", "CADCraft", "#3ea6e8", "计算机辅助设计", "Computer-aided design",
           True, "0.13.0", "0.13.0", False, control="未确认"),
]

UPDATE_STATE = {"checkedAt": "2026-10-27T08:20:47+08:00", "source": "feed",
                "checked": 13, "updates": 1, "errors": []}

IDS = {
    64597592: lambda a: SUITE,
    3380172342: lambda a: UPDATE_STATE,
    3063366872: lambda a: "0.1.0",
    3556255395: lambda a: "/Users/liuming/Library/Application Support/CraftAgent",
    3055576380: lambda a: True,
    2851841750: lambda a: '{\n  "mcpServers": {\n    "craft-agent": {\n      "command": "/Applications/Craft Agent.app/Contents/MacOS/craft-agent",\n      "args": ["mcp"]\n    }\n  }\n}',
    2551660425: lambda a: [],
}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        # 只在本机核对界面时用，写一行到 stderr 便于确认请求到底有没有到。
        sys.stderr.write("%s %s\n" % (self.command, self.path))
        sys.stderr.flush()

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            body = json.loads(raw)
        except Exception:
            body = {}
        # Wails v3 的绑定调用包在 Call 这一层里：
        # {"object": <Call>, "method": 0, "args": [{"call-id": …, "methodID": …}]}
        inner = body.get("args")
        if isinstance(inner, list):
            inner = inner[0] if inner else {}
        if not isinstance(inner, dict):
            inner = {}
        fn = IDS.get(inner.get("methodID"))
        if os.environ.get("MOCK_DEBUG_RAW"):
            sys.stderr.write("raw=%r\n" % (raw[:300],))
        if os.environ.get("MOCK_DEBUG"):
            sys.stderr.write("call methodID=%r methodName=%r hit=%s\n" % (inner.get("methodID"), inner.get("methodName"), bool(fn)))
        payload = fn(inner.get("args")) if fn else None
        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/":
            path = "/index.html"
        full = os.path.normpath(os.path.join(ROOT, path.lstrip("/")))
        if not full.startswith(os.path.abspath(ROOT)) or not os.path.isfile(full):
            self.send_error(404)
            return
        ctype = {".html": "text/html", ".js": "text/javascript", ".css": "text/css",
                 ".ttf": "font/ttf", ".svg": "image/svg+xml"}.get(os.path.splitext(full)[1], "application/octet-stream")
        data = open(full, "rb").read()
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8794
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
