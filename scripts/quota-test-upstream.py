#!/usr/bin/env python3
"""Controlled upstream for quota, streaming and recovery verification.

Run it on a local port, then point channels at http://127.0.0.1:<port>/v1.
Model names select deterministic behaviour:

  ok-model              usage 10 prompt / 5 completion
  no-usage-model        200 JSON without a usage block
  stream-break-model    SSE with one delta and no finish marker / [DONE]
  fail-model            always HTTP 500
  slow-model            sleeps --slow-seconds, then behaves like ok-model
  embed-model           embeddings with usage.prompt_tokens = 10
"""

import argparse
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("port", nargs="?", type=int, default=19090)
    parser.add_argument("--slow-seconds", type=float, default=5.0)
    return parser.parse_args()


ARGS = parse_args()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw or b"{}")
        except Exception:
            body = {}
        if self.path.endswith("/chat/completions"):
            return self.chat(body)
        if self.path.endswith("/embeddings"):
            return self.embeddings(body)
        self.send_json(404, {"error": {"message": "not found"}})

    def chat(self, body):
        model = body.get("model", "unknown")
        if model == "fail-model":
            self.send_json(500, {"error": {"message": "controlled upstream failure"}})
            return
        if model == "slow-model":
            time.sleep(ARGS.slow_seconds)
        if model == "stream-break-model" or body.get("stream"):
            return self.stream(body, model)
        if model == "no-usage-model":
            self.send_json(200, {
                "id": "chatcmpl-no-usage",
                "object": "chat.completion",
                "created": int(time.time()),
                "model": model,
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "no usage block"},
                             "finish_reason": "stop"}],
            })
            return
        self.send_json(200, self.completion(model, prompt=10, completion=5))

    def stream(self, body, model):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        chunks = [
            {"id": "chatcmpl-controlled", "object": "chat.completion.chunk",
             "choices": [{"index": 0, "delta": {"role": "assistant", "content": "partial "}, "finish_reason": None}]},
            {"id": "chatcmpl-controlled", "object": "chat.completion.chunk",
             "choices": [{"index": 0, "delta": {"content": "answer"}, "finish_reason": None}]},
        ]
        for chunk in chunks:
            self.sse(chunk)
        if model == "stream-break-model":
            # Close the chunked body without a finish chunk or [DONE].
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
            return
        self.sse({"id": "chatcmpl-controlled", "object": "chat.completion.chunk",
                  "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                  "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

    def embeddings(self, body):
        self.send_json(200, {
            "object": "list",
            "data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2]}],
            "model": body.get("model", "unknown"),
            "usage": {"prompt_tokens": 10, "total_tokens": 10},
        })

    def completion(self, model, prompt, completion):
        return {
            "id": "chatcmpl-controlled",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "controlled answer"},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": prompt, "completion_tokens": completion,
                      "total_tokens": prompt + completion},
        }

    def sse(self, payload):
        data = json.dumps(payload).encode()
        chunk = b"data: " + data + b"\n\n"
        self.wfile.write(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
        self.wfile.flush()

    def send_json(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


ThreadingHTTPServer(("127.0.0.1", ARGS.port), Handler).serve_forever()
