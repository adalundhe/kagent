# Scratch OpenAI-compatible model for the rv3398 fleet. Not part of any PR.
# Streams a long story one word at a time, so a turn is still streaming when the
# worker is killed. A prompt containing "short" gets a one-line answer instead.
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STORY = ("Once upon a time a lighthouse keeper counted the ships that passed. " * 40).split()

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("[mock] " + (fmt % args) + "\n")

    def do_GET(self):
        body = json.dumps({"data": [{"id": "gpt-4.1-mini", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        last = ""
        for message in request.get("messages", []):
            if message.get("role") == "user":
                content = message.get("content")
                last = content if isinstance(content, str) else json.dumps(content)
        words = ["Short", "answer."] if "short" in last.lower() else STORY
        sys.stderr.write(f"[mock] {self.path} stream={request.get('stream')} last={last[:60]!r} words={len(words)}\n")
        if not request.get("stream"):
            body = json.dumps({"id": "c", "object": "chat.completion", "model": "gpt-4.1-mini",
                "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": " ".join(words)}}],
                "usage": {"prompt_tokens": 1, "completion_tokens": len(words), "total_tokens": 1 + len(words)}}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body); return
        self.send_response(200); self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache"); self.send_header("Connection", "close"); self.end_headers()
        def chunk(delta, finish=None):
            data = {"id": "c", "object": "chat.completion.chunk", "model": "gpt-4.1-mini",
                "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            self.wfile.write(f"data: {json.dumps(data)}\n\n".encode()); self.wfile.flush()
        try:
            chunk({"role": "assistant", "content": ""})
            for word in words:
                chunk({"content": word + " "}); time.sleep(0.25 if len(words) > 2 else 0)
            chunk({}, "stop")
            self.wfile.write(b"data: [DONE]\n\n"); self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            sys.stderr.write("[mock] client went away mid-stream\n")
        self.close_connection = True

ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1]) if len(sys.argv) > 1 else 18398), Handler).serve_forever()
