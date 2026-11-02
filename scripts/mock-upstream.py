import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 19090

CONTENT = {
    "gpt-4o": "Model Relay routes your request through the highest-priority healthy channel and records token usage for every call. You can inspect traffic, spend and latency in the console.",
    "gpt-4o-mini": "Here is a concise summary: the gateway selects an enabled channel that serves the requested model, forwards the OpenAI-compatible payload, and streams the response back without buffering.",
    "o3-mini": "Reasoning response: first I inspected the routing table, then I compared the requested model against each channel's model mapping and health status.",
    "deepseek-chat": "The request was relayed successfully. Usage is attributed to the API key that authenticated the call, so per-key quotas stay accurate.",
    "deepseek-reasoner": "Deep reasoning answer: model aliases let you expose a stable public name while each upstream channel keeps its own provider-specific model identifier.",
    "claude-sonnet-4": "Claude replies through the Anthropic Messages adapter. The gateway translates OpenAI chat requests and streams the Anthropic events back as OpenAI chunks.",
}

OPENAI_MODELS = ["gpt-4.1", "gpt-4o", "gpt-4o-mini", "o1", "o3-mini"]
ANTHROPIC_MODELS = ["claude-haiku-4", "claude-opus-4", "claude-sonnet-4"]
DEEPSEEK_MODELS = ["deepseek-chat", "deepseek-reasoner"]

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def do_GET(self):
        if self.path.endswith("/models"):
            return self.models()
        self.send_json(404, {"error": {"message": "not found"}})

    def models(self):
        auth = self.headers.get("Authorization", "")
        if "reject" in auth or "reject" in self.headers.get("x-api-key", ""):
            return self.send_json(401, {"error": {"message": "invalid api key"}})
        if self.headers.get("x-api-key"):
            ids = ANTHROPIC_MODELS
        elif "deepseek" in auth:
            ids = DEEPSEEK_MODELS
        else:
            ids = OPENAI_MODELS
        self.send_json(200, {"object": "list", "data": [{"id": i, "object": "model", "owned_by": "mock"} for i in ids]})

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
        if self.path.endswith("/messages"):
            return self.messages(body)
        self.send_json(404, {"error": {"message": "not found"}})

    def chat(self, body):
        model = body.get("model", "unknown")
        stream = bool(body.get("stream"))
        content = CONTENT.get(model, f"Hello from the mock upstream for {model}.")
        prompt = max(8, len(json.dumps(body.get("messages", []))) // 4)
        completion = max(4, len(content) // 4)
        if stream:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for index, word in enumerate(content.split(" ")):
                delta = (" " if index else "") + word
                self.sse({"id": "chatcmpl-demo", "object": "chat.completion.chunk", "created": int(time.time()), "model": model,
                          "choices": [{"index": 0, "delta": {"content": delta}, "finish_reason": None}]})
                time.sleep(0.012)
            self.sse({"id": "chatcmpl-demo", "object": "chat.completion.chunk", "created": int(time.time()), "model": model,
                      "choices": [], "usage": {"prompt_tokens": prompt, "completion_tokens": completion,
                                               "total_tokens": prompt + completion}})
            self.sse_raw(b"data: [DONE]\n\n")
            self.wfile.write(b"0\r\n\r\n")
            return
        self.send_json(200, {
            "id": "chatcmpl-demo", "object": "chat.completion", "created": int(time.time()), "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion},
        })

    def messages(self, body):
        model = body.get("model", "claude")
        stream = bool(body.get("stream"))
        content = CONTENT.get(model, "Hello from the mock Anthropic upstream.")
        prompt = max(8, len(json.dumps(body.get("messages", []))) // 4)
        completion = max(4, len(content) // 4)
        if stream:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            self.sse_event("message_start", {"type": "message_start", "message": {"id": "msg_demo", "type": "message", "role": "assistant", "content": [], "usage": {"input_tokens": prompt, "output_tokens": 1}}})
            self.sse_event("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}})
            for word in content.split(" "):
                self.sse_event("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": word + " "}})
                time.sleep(0.012)
            self.sse_event("content_block_stop", {"type": "content_block_stop", "index": 0})
            self.sse_event("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": completion}})
            self.sse_event("message_stop", {"type": "message_stop"})
            self.wfile.write(b"0\r\n\r\n")
            return
        self.send_json(200, {
            "id": "msg_demo", "type": "message", "role": "assistant", "model": model,
            "content": [{"type": "text", "text": content}], "stop_reason": "end_turn",
            "usage": {"input_tokens": prompt, "output_tokens": completion},
        })

    def embeddings(self, body):
        self.send_json(200, {"object": "list", "data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2, 0.3]}],
                             "model": body.get("model", "unknown"), "usage": {"prompt_tokens": 5, "total_tokens": 5}})

    def sse(self, payload):
        data = json.dumps(payload).encode()
        self.sse_raw(b"data: " + data + b"\n\n")

    def sse_raw(self, chunk):
        self.wfile.write(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
        self.wfile.flush()

    def sse_event(self, event, payload):
        data = json.dumps(payload).encode()
        chunk = b"event: " + event.encode() + b"\ndata: " + data + b"\n\n"
        self.wfile.write(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
        self.wfile.flush()

    def send_json(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
