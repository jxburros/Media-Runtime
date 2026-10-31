# Test certificates for remote workers and API network mode

Fixture material for the network-mode tests in `internal/integration` and `cmd/media-runtime`.
The integration suite launches Media Engine's fixture worker (`tests/serve_fixture.py`) in network
mode with `worker.pem`/`worker.key` and `--tls-client-ca ca.pem`, and the runtime connects to it
with `client.pem`/`client.key`; the runtime API itself serves `runtime.pem`/`runtime.key`. They are
**test-only**: the CA private key was discarded after signing, nothing outside these tests trusts
this CA, and the private keys below are public by being committed. Never use them for a real
worker or runtime.

| File                                   | What it is                                                                  |
| -------------------------------------- | --------------------------------------------------------------------------- |
| `ca.pem`                               | Test CA (`CN=Media Runtime test CA`); the worker's `--tls-client-ca`, the runtime's `tls.ca_file` and the CLI's `MEDIA_RUNTIME_CA` |
| `worker.pem`, `worker.key`             | Worker server certificate signed by the CA; SAN `IP:127.0.0.1`, `DNS:localhost`; `serverAuth` |
| `worker-rotated.pem`, `worker-rotated.key` | A second worker server certificate with the same names and a new key: a rotation, or an impostor holding a certificate from the same CA (its fingerprint differs) |
| `runtime.pem`, `runtime.key`           | Runtime API server certificate signed by the CA; SAN `IP:127.0.0.1`, `DNS:localhost`; `serverAuth` |
| `client.pem`, `client.key`             | The runtime's client certificate for mutual TLS to the worker; `clientAuth` |
| `extensions.cnf`                       | X.509 extensions used by the commands below                                 |

All keys are EC P-256; every certificate is valid for 10 years (3650 days, from 2026-10-29).
They are generated once and committed; tests never generate certificates.

Git does not record file modes, so a checkout has the keys at your umask (usually 0644). Both the
runtime and the worker refuse a private key readable by group or others, so the tests copy the
files to a private directory (keys 0600, certificates 0644) before using them.

## Regenerating

From this directory, with OpenSSL 3:

```sh
set -e
key() { openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1.key"; }
key ca
openssl req -new -x509 -key ca.key -subj "/CN=Media Runtime test CA" -days 3650 -sha256 \
  -extensions ca -config extensions.cnf -out ca.pem
for pair in worker:server worker-rotated:server runtime:server client:client; do
  name=${pair%%:*}; ext=${pair##*:}
  key $name
  openssl req -new -key $name.key -subj "/CN=media-runtime-test-$name" -out $name.csr
  openssl x509 -req -in $name.csr -CA ca.pem -CAkey ca.key -set_serial 0x$(openssl rand -hex 16) \
    -days 3650 -sha256 -extfile extensions.cnf -extensions $ext -out $name.pem
  rm $name.csr
done
rm ca.key
openssl verify -CAfile ca.pem worker.pem worker-rotated.pem runtime.pem client.pem
```

Regenerating changes every fingerprint; nothing in the tests pins a literal one (they compare
against the SHA-256 of the certificate's DER, computed from the file).
