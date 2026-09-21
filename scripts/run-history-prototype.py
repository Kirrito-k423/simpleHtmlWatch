#!/usr/bin/env python3
"""THROWAWAY / local UI server. No SSH, saved credentials, or history persistence.
Run: python3 scripts/run-history-prototype.py [port]
"""
import json
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import sys
from urllib.parse import urlparse

WEB = Path(__file__).resolve().parents[1] / 'web'
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 19877

class PrototypeHandler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(WEB), **kwargs)

    def do_GET(self):
        path = urlparse(self.path).path
        if path == '/':
            data = (WEB / 'index.html').read_text().replace('__WATCH_TOKEN__', 'prototype-only').replace('__HISTORY_PROTOTYPE__', 'true').encode()
            content_type = 'text/html; charset=utf-8'
        elif path == '/api/config':
            data = json.dumps({'config': {'interval': 4, 'autoTrustNewKeys': False, 'profiles': [], 'machines': [], 'customCommands': []}, 'commands': [{'id': 'npu', 'name': 'NPU 状态'}, {'id': 'python', 'name': 'Python 进程'}, {'id': 'usage', 'name': 'CPU / 内存'}]}).encode()
            content_type = 'application/json'
        elif path == '/api/status':
            data, content_type = b'{}', 'application/json'
        else:
            return super().do_GET()
        self.send_response(200)
        self.send_header('Content-Type', content_type)
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass

print(f'PROTOTYPE — 模拟数据 / 内存记录 / 不连接 SSH\nhttp://127.0.0.1:{PORT}/?demo=1&prototype=history&variant=C', flush=True)
ThreadingHTTPServer(('127.0.0.1', PORT), PrototypeHandler).serve_forever()
