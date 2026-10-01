#!/bin/bash
set -eu
source /home/isucon/env.sh
printf '%s' "${ISUCON13_SESSION_SECRETKEY-isucon13_session_cookiestore_defaultsecret}" | sha256sum
