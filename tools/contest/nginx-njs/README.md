# Icon edge module

`task build-icon-edge-module` builds njs 0.8.9 as a dynamic module for the
installed nginx 1.18.0, inside a local Linux/amd64 Ubuntu 22.04 container.
The upstream source archives are pinned by `sources.sha256`.

`nginx-1.18-compat.patch` adapts only `ngx.error_log_path`: nginx 1.18 keeps
the log file name on `ngx_cycle->log->file`, rather than `ngx_cycle->error_log`.
The nginx binary is not rebuilt or replaced. The build checks module loading
against Ubuntu nginx; deployment separately checks the actual installed binary
before activating any configuration. The module is deployed to
`/etc/nginx/ngx_http_js_module.so`; configuration rsync excludes that artifact.

`icon-native.patch` adds the contest-specific directive
`isupipe_icon_native icon_hashes icon_control`. Its C content handler reads the
same njs dictionaries without cloning or starting a request VM. Each shared
value is consumed under its dictionary read lock; the publication identity is
checked before and after the hash read, and the original wall-clock lease is
checked before returning 304. Unsupported validators, unknown users, expired
leases and non-GET methods use `@icon_reader`. Cookie decoding is only for the
existing opaque log identity; it does not authenticate a request.

The njs control handlers still own full/delta publication and invalidation.
`task deploy-viewer-latency` deploys the Go timeline gzip implementation to
isucon-3 and restarts nginx on isucon-1 and isucon-2. A module replacement needs
a master restart: reloading an existing master can retain the old loaded module.
`task deploy-viewer-latency-dry` prints the exact targets and activation scripts.

The Go gzip path covers only successful comment/reaction timeline GETs. It
negotiates `Accept-Encoding`, retains `Vary: Accept-Encoding`, and uses pooled
BestSpeed writers. Bodies below 1 KiB or bodies that do not shrink remain plain
JSON. Pretty JSON and the trailing newline are preserved. Oversized compressor
output buffers are not retained in the pool. The authenticated ingress smoke
compares identity JSON with decoded gzip JSON through both actual nginx hosts.

`task check-icon-edge-local` uses two real nginx workers and the Linux Go
reader against synthetic image data. It checks direct 304, session logging,
lost acknowledgements, capacity failure, lease expiration, notifications,
generation replacement, restart/reload, and the authoritative backup path.
It does not execute the contest benchmark.

The full snapshot publish endpoint keeps bounded request bodies in memory
(`client_body_buffer_size 4m`) because njs `r.requestText` does not expose bodies
spooled to a temporary file.

Use `task deploy-icon-edge` for the complete deployment and verification.
For a standalone `task deploy-nginx` from a clean checkout, first run
`task build-icon-edge-module`.
