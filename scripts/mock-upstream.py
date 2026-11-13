import json, os, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 19090
MEDIA_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "media")
VIDEO_TASKS = {}
VIDEO_LOCK = threading.Lock()
VOICE_DESIGNS = {}

SYSTEM_VOICES = [
    {"voice_id": "English_expressive_narrator", "voice_name": "Expressive Narrator",
     "description": ["A clear, expressive English narrator."]},
    {"voice_id": "English_Graceful_Lady", "voice_name": "Graceful Lady",
     "description": ["A calm and graceful English voice."]},
    {"voice_id": "English_magnetic_voiced_man", "voice_name": "Magnetic-voiced Male",
     "description": ["A deep, magnetic English voice."]},
    {"voice_id": "Chinese (Mandarin)_Reliable_Executive", "voice_name": "Reliable Executive",
     "description": ["A steady Mandarin executive voice."]},
    {"voice_id": "Chinese (Mandarin)_News_Anchor", "voice_name": "News Anchor",
     "description": ["A broadcast-style Mandarin news anchor."]},
    {"voice_id": "Japanese_IntellectualSenior", "voice_name": "Intellectual Senior",
     "description": ["A thoughtful Japanese voice."]},
    {"voice_id": "Korean_CalmGentleman", "voice_name": "Calm Gentleman",
     "description": ["A calm Korean voice."]},
    {"voice_id": "Cantonese_GentleLady", "voice_name": "Gentle Lady",
     "description": ["A gentle Cantonese voice."]},
]

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
        if self.path.startswith("/media/") and (self.path.endswith(".mp4") or self.path.endswith(".mp3")):
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
        if self.path.endswith("/v1/get_voice"):
            return self.get_voice()
        if self.path.endswith("/v1/t2a_v2"):
            return self.text_to_speech(body)
        if self.path.endswith("/v1/voice_design"):
            return self.voice_design(body)
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

    def get_voice(self):
        if self.headers.get("Authorization") != "Bearer upstream-key":
            return self.send_json(401, {"base_resp": {"status_code": 1004, "status_msg": "login fail"}})
        self.send_json(200, {
            "system_voice": SYSTEM_VOICES,
            "base_resp": {"status_code": 0, "status_msg": "success"},
        })

    def text_to_speech(self, body):
        if self.headers.get("Authorization") != "Bearer upstream-key":
            return self.send_json(401, {"base_resp": {"status_code": 1004, "status_msg": "login fail"}})
        text = str(body.get("text", ""))
        if not text.strip():
            return self.send_json(200, {"data": None, "base_resp": {"status_code": 2013, "status_msg": "text is required"}})
        if "error" in text.lower():
            return self.send_json(200, {"data": None, "base_resp": {"status_code": 2013, "status_msg": "the text was rejected by the mock upstream"}})
        with open(os.path.join(MEDIA_DIR, "demo.mp3"), "rb") as handle:
            audio = handle.read()
        # scripts/media/demo.mp3 is a seven-second sample reused for every mock
        # synthesis, so report its real play length.
        duration = 7440
        sys.stderr.write("mock t2a_v2 voice=%s chars=%d\n" % (
            body.get("voice_setting", {}).get("voice_id"), len(text)))
        self.send_json(200, {
            "data": {"audio": audio.hex(), "status": 2},
            "extra_info": {
                "audio_length": duration,
                "audio_sample_rate": 32000,
                "audio_size": len(audio),
                "bitrate": 64000,
                "word_count": len(text),
                "usage_characters": len(text),
                "audio_format": "mp3",
                "audio_channel": 1,
            },
            "trace_id": "mock-t2a",
            "base_resp": {"status_code": 0, "status_msg": "success"},
        })

    def voice_design(self, body):
        if self.headers.get("Authorization") != "Bearer upstream-key":
            return self.send_json(401, {"base_resp": {"status_code": 1004, "status_msg": "login fail"}})
        prompt = str(body.get("prompt", "")).strip()
        preview = str(body.get("preview_text", "")).strip()
        if not prompt or not preview:
            return self.send_json(200, {"base_resp": {"status_code": 2013, "status_msg": "prompt and preview_text are required"}})
        if "error" in prompt.lower() or "error" in preview.lower():
            return self.send_json(200, {"base_resp": {"status_code": 2013, "status_msg": "the description was rejected by the mock upstream"}})
        with VIDEO_LOCK:
            voice_id = str(body.get("voice_id", "")).strip() or "ttv-voice-mock-%04d" % (len(VOICE_DESIGNS) + 1)
            VOICE_DESIGNS[voice_id] = {"prompt": prompt, "preview_text": preview}
        with open(os.path.join(MEDIA_DIR, "demo.mp3"), "rb") as handle:
            audio = handle.read()
        sys.stderr.write("mock voice_design voice=%s prompt=%r\n" % (voice_id, prompt[:60]))
        self.send_json(200, {
            "voice_id": voice_id,
            "trial_audio": audio.hex(),
            "base_resp": {"status_code": 0, "status_msg": "success"},
        })

    def media(self):
        name = "demo.mp3" if self.path.endswith(".mp3") else "demo.mp4"
        path = os.path.join(MEDIA_DIR, name)
        try:
            with open(path, "rb") as handle:
                data = handle.read()
        except OSError:
            return self.send_json(404, {"error": {"message": "demo media is missing"}})
        self.send_response(200)
        self.send_header("Content-Type", "audio/mpeg" if name.endswith(".mp3") else "video/mp4")
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
