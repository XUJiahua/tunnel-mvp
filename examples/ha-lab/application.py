"""Disposable, private application fixture; never a production application."""
import base64
import hashlib
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

lock = threading.Lock()
commits = 0


class Application(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def send(self, body, content_type="text/plain"):
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/events":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            try:
                for sequence in range(3000):
                    self.wfile.write(f"data: {sequence}\n\n".encode())
                    self.wfile.flush()
                    time.sleep(0.2)
            except (BrokenPipeError, ConnectionResetError):
                pass
        elif self.path == "/ws" and self.headers.get("Upgrade", "").lower() == "websocket":
            key = self.headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", base64.b64encode(hashlib.sha1(key.encode()).digest()).decode())
            self.end_headers()
            # One short masked client frame, echoed once, then close.
            header = self.rfile.read(2)
            if len(header) != 2 or not header[1] & 128 or header[1] & 127 > 125:
                return
            mask = self.rfile.read(4)
            data = self.rfile.read(header[1] & 127)
            if len(mask) != 4 or len(data) != header[1] & 127:
                return
            plain = bytes(byte ^ mask[i % 4] for i, byte in enumerate(data))
            self.wfile.write(bytes([0x81, len(plain)]) + plain)
            self.wfile.flush()
            self.close_connection = True
        elif self.path == "/count":
            with lock:
                body = json.dumps({"commits": commits}).encode()
            self.send(body, "application/json")
        else:
            self.send(b"ha-deployment-live")

    def do_POST(self):
        global commits
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        if self.path != "/commit":
            self.send_error(404)
            return
        with lock:
            commits += 1
        self.send(body)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Application).serve_forever()
