import json, os, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 19090
MEDIA_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "media", "demo.mp4")
VIDEO_TASKS = {}
VIDEO_LOCK = threading.Lock()

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
SEEDANCE_MODELS = ["doubao-seedance-2-5-260628", "doubao-seedance-2-0-260128",
                   "doubao-seedance-1-0-pro-250528", "doubao-seedance-1-0-pro-fast-251015",
                   "doubao-seedance-1-0-lite-i2v-250428"]

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def do_GET(self):
        if self.path.startswith("/v2/query/video_generation/"):
            return self.query_video(self.path.rsplit("/", 1)[-1])
        if self.path.startswith("/api/v3/contents/generations/tasks/"):
            return self.query_seedance(self.path.rsplit("/", 1)[-1])
        if self.path.startswith("/media/") and self.path.endswith(".mp4"):
            return self.media()
        if self.path.endswith("/models"):
            return self.models()
        self.send_json(404, {"error": {"message": "not found"}})

    def models(self):
        auth = self.headers.get("Authorization", "")
        if "reject" in auth or "reject" in self.headers.get("x-api-key", ""):
            return self.send_json(401, {"error": {"message": "invalid api key"}})
        if self.path.startswith("/api/v3/"):
            ids = SEEDANCE_MODELS
        elif self.headers.get("x-api-key"):
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
        if self.path.endswith("/v2/video_generation"):
            return self.create_video(body)
        if self.path.endswith("/contents/generations/tasks"):
            return self.create_seedance(body)
        self.send_json(404, {"error": {"message": "not found"}})

    def create_video(self, body):
        if self.headers.get("Authorization") != "Bearer upstream-key":
            return self.send_json(401, {"type": "error", "error": {
                "type": "authorized_error", "message": "login fail (1004)", "http_code": "401"}})
        with VIDEO_LOCK:
            task_id = "task-%d" % (len(VIDEO_TASKS) + 1)
            VIDEO_TASKS[task_id] = {
                "body": body,
                "queries": 0,
                "created_at": int(time.time()),
            }
        sys.stderr.write("mock minimax task %s: %s\n" % (task_id, body.get("model")))
        self.send_json(200, {"task_id": task_id})

    def query_video(self, task_id):
        with VIDEO_LOCK:
            task = VIDEO_TASKS.get(task_id)
            if task is None:
                return self.send_json(400, {"type": "error", "error": {
                    "type": "bad_request_error", "message": "invalid task_id (2013)", "http_code": "400"}})
            task["queries"] += 1
            queries = task["queries"]
            body = task["body"]
            created_at = task["created_at"]
        status = "running" if queries < 2 else "succeeded"
        prompt = next((item.get("text", "") for item in body.get("content", []) if item.get("type") == "text"), "")
        if status == "succeeded" and "fail" in prompt.lower():
            status = "failed"
        payload = {
            "id": task_id,
            "model": body.get("model", "MiniMax-H3"),
            "status": status,
            "created_at": created_at,
            "updated_at": int(time.time()),
            "resolution": body.get("resolution", "768P"),
            "duration": body.get("duration", 5),
            "ratio": body.get("ratio", "16:9"),
            "task_type": "generation",
            "modality": "video",
        }
        if status == "succeeded":
            payload["content"] = {"url": "http://127.0.0.1:%d/media/%s.mp4" % (PORT, task_id)}
            payload["usage"] = {
                "total_seconds": body.get("duration", 5),
                "input_seconds": 0,
                "output_seconds": body.get("duration", 5),
                "input_image_count": sum(1 for item in body.get("content", []) if item.get("type") == "image_url"),
                "total_tokens": 25000,
                "prompt_tokens": 1500,
                "completion_tokens": 23500,
            }
        elif status == "failed":
            payload["error"] = {"code": "1026", "message": "video description contains sensitive content"}
        self.send_json(200, {"task": payload})

    def create_seedance(self, body):
        if self.headers.get("Authorization") != "Bearer ark-key":
            return self.send_json(401, {"error": {
                "code": "AuthenticationError",
                "message": "the API key or AK/SK in the request is missing or invalid.",
            }})
        with VIDEO_LOCK:
            task_id = "cgt-%d" % (len(VIDEO_TASKS) + 1)
            VIDEO_TASKS[task_id] = {
                "provider": "seedance",
                "body": body,
                "queries": 0,
                "created_at": int(time.time()),
            }
        sys.stderr.write("mock seedance task %s: %s\n" % (task_id, body.get("model")))
        self.send_json(200, {"id": task_id})

    def query_seedance(self, task_id):
        with VIDEO_LOCK:
            task = VIDEO_TASKS.get(task_id)
            if task is None or task.get("provider") != "seedance":
                return self.send_json(404, {"error": {
                    "code": "InvalidParameter",
                    "message": "the requested task does not exist",
                }})
            task["queries"] += 1
            queries = task["queries"]
            body = task["body"]
            created_at = task["created_at"]
        status = "running" if queries < 2 else "succeeded"
        prompt = next((item.get("text", "") for item in body.get("content", []) if item.get("type") == "text"), "")
        if status == "succeeded" and "fail" in prompt.lower():
            status = "failed"
        payload = {
            "id": task_id,
            "model": body.get("model", "doubao-seedance-1-0-pro-250528"),
            "status": status,
            "created_at": created_at,
            "updated_at": int(time.time()),
            "resolution": body.get("resolution", "720p"),
            "duration": body.get("duration", 5),
            "ratio": body.get("ratio", "16:9"),
        }
        if status == "succeeded":
            payload["content"] = {"video_url": "http://127.0.0.1:%d/media/%s.mp4" % (PORT, task_id)}
        elif status == "failed":
            payload["error"] = {"code": "OutputVideoSensitiveContentDetected",
                                "message": "the generated video was blocked by the safety filter"}
        self.send_json(200, payload)

    def media(self):
        try:
            with open(MEDIA_PATH, "rb") as handle:
                data = handle.read()
        except OSError:
            return self.send_json(404, {"error": {"message": "demo video is missing"}})
        self.send_response(200)
        self.send_header("Content-Type", "video/mp4")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

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
