import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from media_runtime import Client, RuntimeError


class ClientTests(unittest.TestCase):
    def test_http_auth_idempotency_errors_and_sse_resume(self):
        observed = []
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args): pass
            def do_POST(self):
                observed.append((self.headers.get("Authorization"), self.headers.get("Idempotency-Key")))
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                data = json.dumps({"job_id": "job_test", "state": "queued"}).encode()
                self.send_response(202); self.end_headers(); self.wfile.write(data)
            def do_GET(self):
                if self.path.endswith("/events"):
                    observed.append(self.headers.get("Last-Event-ID"))
                    self.send_response(200); self.end_headers()
                    self.wfile.write(b'id: 8\nevent: job_state\ndata: {"state":"succeeded"}\n\n')
                else:
                    self.send_response(404); self.end_headers()
                    self.wfile.write(b'{"error":{"code":"not_found","message":"Missing job"}}')
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        try:
            client = Client(f"http://127.0.0.1:{server.server_port}", "token")
            self.assertEqual(client.submit({"recipe_id": "portrait"}, "key")["job_id"], "job_test")
            self.assertEqual(observed[0], ("Bearer token", "key"))
            self.assertEqual(list(client.events("job_test", 7))[0]["id"], 8)
            self.assertEqual(observed[1], "7")
            with self.assertRaises(RuntimeError) as failure: client.status("missing")
            self.assertEqual(failure.exception.code, "not_found")
        finally:
            server.shutdown(); server.server_close(); thread.join()

    def test_plain_remote_urls_are_refused(self):
        with self.assertRaises(ValueError): Client("http://192.168.1.4:7810", "token")


if __name__ == "__main__": unittest.main()
