"""Exercise the actual nginx module and Go reader with a synthetic HTTP owner."""
import base64
import hashlib
import hmac
import http.client
import json
import os
import struct
import subprocess
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

lock = threading.Lock()
owner = dict(epoch="owner-one", revision=0, changes=[], users={"alpha": b"image-one"}, paused=False)


class Origin(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        with lock:
            if self.path.startswith("/_internal/icon-state"):
                if owner["paused"]:
                    self.send_response(503)
                    self.end_headers()
                    return
                from urllib.parse import parse_qs, urlparse
                args = parse_qs(urlparse(self.path).query)
                after = int(args.get("after", ["0"])[0])
                full = args.get("epoch", [""])[0] != owner["epoch"] or after > owner["revision"]
                changes = ([dict(name=n, hash=hashlib.sha256(b).hexdigest()) for n, b in owner["users"].items()]
                           if full else owner["changes"][after:])
                data = json.dumps(dict(epoch=owner["epoch"], revision=owner["revision"], full=full, changes=changes)).encode()
                self.send_response(200)
                self.send_header("Content-Length", len(data))
                self.end_headers()
                self.wfile.write(data)
                return
            name = self.path.split("/")[-2]
            body = owner["users"].get(name)
        if body is None:
            self.send_response(404)
            self.end_headers()
            return
        etag = '"' + hashlib.sha256(body).hexdigest() + '"'
        match = etag in self.headers.get("If-None-Match", "")
        self.send_response(304 if match else 200)
        self.send_header("ETag", etag)
        self.send_header("Content-Type", "image/jpeg")
        self.send_header("Content-Length", 0 if match else len(body))
        self.end_headers()
        if not match:
            self.wfile.write(body)


def request(port, path, method="GET", value=None, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
    data = json.dumps(value) if value is not None else None
    conn.request(method, path, data, headers or {})
    response = conn.getresponse()
    result = response.status, dict(response.getheaders()), response.read()
    conn.close()
    return result


def wait(predicate, message, seconds=3):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        try:
            if predicate():
                return
        except (OSError, ValueError, http.client.HTTPException):
            pass
        time.sleep(.02)
    raise AssertionError(message)


def status():
    code, _, body = request(8055, "/status")
    assert code == 200
    return json.loads(body)


def publication(changes, mode="full", epoch="manual", revision=0):
    current = status()
    return dict(base_publish_id=current["publish_id"], publish_id=str(uuid.uuid4()),
                mode=mode, epoch=epoch, base_revision=current["revision"], revision=revision,
                valid_until_ms=int(time.time()*1000)+700, changes=changes)


def publish(payload, expected=200):
    code, _, _ = request(8055, "/publish", "POST", payload)
    assert code == expected, ("publish", code, expected)


def get_icon(etag=None, name="alpha", cookie=None):
    headers = {}
    if etag:
        headers["If-None-Match"] = etag
    if cookie:
        headers["Cookie"] = "SESSIONID=" + cookie
    return request(8081, "/api/user/" + name + "/icon", headers=headers)


def mutate(body, notify=True):
    with lock:
        owner["users"]["alpha"] = body
        owner["revision"] += 1
        change = dict(name="alpha", hash=hashlib.sha256(body).hexdigest())
        owner["changes"].append(change)
        update = dict(epoch=owner["epoch"], revision=owner["revision"], **change)
    if notify:
        assert request(8054, "/_internal/update", "POST", update)[0] == 204
    return '"' + change["hash"] + '"'


config = '''
load_module /build/nginx-1.18.0/objs/ngx_http_js_module.so;
worker_processes 2;
pid /tmp/icon-nginx.pid;
error_log /tmp/icon-error.log;
events { worker_connections 128; }
http {
    js_import icon from /etc/nginx/njs/icon.js;
    js_shared_dict_zone zone=icon_hashes:64k;
    js_shared_dict_zone zone=icon_control:64k;
    js_var $icon_session_id;
    log_format check '$status|$upstream_addr|$icon_session_id';
    access_log /tmp/icon-access.log check;
    upstream icon_read {
        server unix:/tmp/icon-reader.sock fail_timeout=1s;
        server 127.0.0.1:8080 backup;
        keepalive 8;
    }
    server {
        listen 127.0.0.1:8055;
        client_max_body_size 4m;
        client_body_buffer_size 4m;
        access_log off;
        location = /status { js_content icon.status; }
        location = /publish { js_content icon.publish; }
    }
    server {
        listen 8081;
        location ~ ^/api/user/(?<icon_username>[^/]+)/icon$ { isupipe_icon_native icon_hashes icon_control; }
        location @icon_reader {
            proxy_http_version 1.1;
            proxy_set_header Connection "";
            proxy_pass http://icon_read;
        }
    }
}
'''
with open("/tmp/icon-nginx.conf", "w") as f:
    f.write(config)
nginx = None
reader = None
server = ThreadingHTTPServer(("127.0.0.1", 8080), Origin)
threading.Thread(target=server.serve_forever, daemon=True).start()


def start_nginx():
    return subprocess.Popen(["nginx", "-c", "/tmp/icon-nginx.conf", "-g", "daemon off;"])


def start_reader():
    env = dict(os.environ, ISUPIPE_MODE="icon-read", ISUPIPE_ICON_ORIGIN_URL="http://127.0.0.1:8080",
               ISUPIPE_ICON_READ_LISTEN="127.0.0.1:8054", ISUPIPE_ICON_READ_SOCKET="/tmp/icon-reader.sock",
               ISUPIPE_ICON_EDGE_URL="http://127.0.0.1:8055")
    return subprocess.Popen(["/app/isupipe"], env=env)


try:
    nginx = start_nginx()
    wait(lambda: status()["dirty"], "nginx startup")
    first_hash = hashlib.sha256(b"image-one").hexdigest()
    first = '"' + first_hash + '"'
    publish(publication([dict(name="alpha", hash=first_hash)]))
    payload = b"\x01" + struct.pack(">qq", 1, int(time.time())+3600) + b"nonce-for-tests!"
    assert len(payload) == 33
    signature = hmac.new(b"isucon13_session_cookiestore_defaultsecret", b"isupipe-session-v1:"+payload, hashlib.sha256).digest()
    cookie = base64.urlsafe_b64encode(payload+signature).decode().rstrip("=")
    assert get_icon(first, cookie=cookie)[0] == 304
    wait(lambda: "304|-|"+signature.hex() in open("/tmp/icon-access.log").read(), "direct 304 and session identity")
    for invalid in ["bad", cookie + "=", "!" * 87,
                    base64.urlsafe_b64encode(payload[:9] + bytes(8) + payload[17:] + signature).decode().rstrip("=")]:
        before_lines = len(open("/tmp/icon-access.log").read().splitlines())
        assert get_icon(first, cookie=invalid)[0] == 304
        wait(lambda: len(open("/tmp/icon-access.log").read().splitlines()) > before_lines,
             "invalid session request logged")
        assert open("/tmp/icon-access.log").read().splitlines()[-1] == "304|-|"
    conn = http.client.HTTPConnection("127.0.0.1", 8081, timeout=3)
    for _ in range(3):
        conn.request("GET", "/api/user/alpha%20/icon", headers={"If-None-Match": first})
        response = conn.getresponse()
        assert response.status == 304 and response.getheader("ETag") == first
        assert response.read() == b""
    conn.close()
    assert get_icon()[0] == 200
    assert get_icon('"' + '0'*64 + '"')[0] == 200
    assert get_icon(first, "missing")[0] == 404
    assert get_icon("W/"+first)[0] == 304
    print("PASS native 304, image, unknown user, special ETag, session validation and keepalive", flush=True)

    renewal = publication([], mode="delta")
    publish(renewal)
    assert status()["fresh"] and status()["publish_id"] == renewal["publish_id"]
    assert get_icon(first)[0] == 304
    wait(lambda: open("/tmp/icon-access.log").read().splitlines()[-1] == "304|-|",
         "unchanged renewal keeps native 304")
    print("PASS unchanged lease renewal with native 304", flush=True)

    retry = publication([dict(name="alpha", hash=first_hash)])
    publish(retry)
    publish(retry)
    newer = publication([dict(name="alpha", hash=first_hash)])
    publish(newer)
    publish(retry, 409)
    overflow = publication([dict(name="name-"+str(i), hash=first_hash) for i in range(3000)])
    publish(overflow, 503)
    assert status()["dirty"] and not status()["fresh"]
    assert get_icon(first)[0] == 304
    publish(publication([dict(name="alpha", hash=first_hash)]))
    time.sleep(.8)
    assert not status()["fresh"]
    assert get_icon(first)[0] == 304
    assert "127.0.0.1:8080" in open("/tmp/icon-access.log").read().splitlines()[-1]
    print("PASS lost ACK retry, old cursor rejection, capacity failure, lease expiration", flush=True)

    reader = start_reader()
    wait(lambda: status()["epoch"] == owner["epoch"] and status()["fresh"], "reader full publication")
    second = mutate(b"image-two")
    wait(lambda: status()["revision"] == 1 and status()["fresh"], "delta publication")
    assert get_icon(first)[0] == 200 and get_icon(first)[2] == b"image-two"
    assert get_icon(second)[0] == 304
    print("PASS real Go synchronization, notification, delta, updated image", flush=True)

    with lock:
        owner["paused"] = True
    third = mutate(b"image-three", notify=False)
    wait(lambda: not status()["fresh"], "missed update expiration", seconds=2)
    assert get_icon(second)[0] == 200 and get_icon(second)[2] == b"image-three"
    with lock:
        owner["paused"] = False
    wait(lambda: status()["revision"] == 2 and status()["fresh"], "missed delta repair")
    print("PASS missed notification and unavailable snapshots never renew stale metadata", flush=True)

    subprocess.run(["nginx", "-c", "/tmp/icon-nginx.conf", "-s", "reload"], check=True)
    assert get_icon(third)[0] == 304
    reader.terminate()
    reader.wait(timeout=3)
    time.sleep(.8)
    assert get_icon(third)[0] == 304
    reader = start_reader()
    wait(lambda: status()["fresh"], "reader restart")
    nginx.terminate()
    nginx.wait(timeout=3)
    nginx = start_nginx()
    wait(lambda: status()["epoch"] == owner["epoch"] and status()["fresh"], "nginx restart rebuild")
    assert get_icon(third)[0] == 304
    print("PASS nginx reload/restart, Go restart, owner backup", flush=True)

    with lock:
        owner.update(epoch="owner-two", revision=0, changes=[], users={"beta": b"new-generation"})
    assert request(8054, "/_internal/invalidate", "POST", {})[0] == 204
    wait(lambda: status()["epoch"] == "owner-two" and status()["fresh"], "new generation")
    assert get_icon(third)[0] == 404
    assert status()["users"] == 1
    print("PASS generation reset removes prior users", flush=True)
finally:
    for process in (reader, nginx):
        if process is not None and process.poll() is None:
            process.terminate()
            process.wait(timeout=3)
    server.shutdown()
    if os.path.exists("/tmp/icon-error.log"):
        errors = open("/tmp/icon-error.log").read()
        if "js exception" in errors:
            print(errors)
            raise AssertionError("nginx JavaScript exception")
