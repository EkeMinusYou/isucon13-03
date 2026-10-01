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
