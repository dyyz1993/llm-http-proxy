#!/usr/bin/env python3
"""最小 echo 后端:回显请求的 method/path/body/Authorization。
供 Docker CI 自验证使用。监听 8910。
"""
import http.server
import json


class Handler(http.server.BaseHTTPRequestHandler):
    def _handle(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(n).decode() if n else ""
        out = json.dumps({
            "method": self.command,
            "path": self.path,
            "body": body,
            "auth": self.headers.get("Authorization", ""),
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def do_POST(self):
        self._handle()

    def do_GET(self):
        self._handle()

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    http.server.HTTPServer(("0.0.0.0", 8910), Handler).serve_forever()
