function state() {
    const encoded = ngx.shared.icon_control.get('state');
    return encoded ? JSON.parse(encoded) : {
        publish_id: '', epoch: '', revision: 0, dirty: true, valid_until_ms: 0
    };
}

function status(r) {
    const result = state();
    result.locked = ngx.shared.icon_control.has('writer');
    result.users = ngx.shared.icon_hashes.size();
    result.free_bytes = ngx.shared.icon_hashes.freeSpace();
    const active = ngx.shared.icon_control.get('active');
    result.fresh = !!active && Date.now() < result.valid_until_ms;
    r.headersOut['Content-Type'] = 'application/json';
    r.return(200, JSON.stringify(result));
}

function validRequest(p) {
    if (!p || typeof p.base_publish_id !== 'string' ||
        typeof p.publish_id !== 'string' || !/^[0-9a-f-]{36}$/.test(p.publish_id) ||
        !['full', 'delta', 'invalidate'].includes(p.mode)) return false;
    if (p.mode === 'invalidate') return true;
    if (typeof p.epoch !== 'string' || !p.epoch ||
        !Number.isSafeInteger(p.revision) || p.revision < 0 ||
        !Number.isSafeInteger(p.base_revision) || p.base_revision < 0 ||
        !Number.isSafeInteger(p.valid_until_ms) ||
        !Array.isArray(p.changes)) return false;
    return p.changes.every(c => c && typeof c.name === 'string' && c.name &&
        !c.name.includes('/') && !c.name.endsWith(' ') &&
        typeof c.hash === 'string' && /^[0-9a-f]{64}$/.test(c.hash));
}

function publish(r) {
    if (r.method !== 'POST') { r.return(405); return; }
    let p;
    try { p = JSON.parse(r.requestText); } catch (_) { r.return(400); return; }
    if (!validRequest(p)) { r.return(400); return; }
    const control = ngx.shared.icon_control;
    if (!control.add('writer', p.publish_id)) { r.return(409); return; }
    try {
        const current = state();
        if (current.publish_id === p.publish_id) { status(r); return; }
        if (current.publish_id !== p.base_publish_id) { r.return(409); return; }
        if (p.mode !== 'invalidate') {
            const now = Date.now();
            if (p.valid_until_ms <= now || p.valid_until_ms > now + 750) {
                r.return(409); return;
            }
            if (p.mode === 'delta' && (current.dirty || current.epoch !== p.epoch ||
                current.revision !== p.base_revision || p.revision < current.revision)) {
                r.return(409); return;
            }
        }
        // Close publication before any entry can change; failures keep it closed.
        control.delete('active');
        current.dirty = true;
        current.valid_until_ms = 0;
        control.set('state', JSON.stringify(current));
        if (p.mode === 'invalidate') {
            control.set('state', JSON.stringify({publish_id: p.publish_id,
                epoch: '', revision: 0, dirty: true, valid_until_ms: 0}));
            status(r);
            return;
        }
        if (p.mode === 'full') ngx.shared.icon_hashes.clear();
        for (let i = 0; i < p.changes.length; i++) {
            const change = p.changes[i];
            ngx.shared.icon_hashes.set(change.name, change.hash);
        }
        const next = {publish_id: p.publish_id, epoch: p.epoch,
            revision: p.revision, dirty: false, valid_until_ms: p.valid_until_ms};
        control.set('state', JSON.stringify(next));
        control.set('active', p.publish_id + ':' + p.valid_until_ms);
        status(r);
    } catch (_) {
        control.delete('active');
        const current = state();
        current.dirty = true;
        current.valid_until_ms = 0;
        control.set('state', JSON.stringify(current));
        r.return(503);
    } finally {
        control.delete('writer');
    }
}

function measurementID(r) {
    // Public GET logging only; authenticated APIs still verify the signature in Go.
    const value = r.variables.cookie_SESSIONID;
    if (!value || value.length !== 87 || !/^[A-Za-z0-9_-]+$/.test(value)) return '';
    const token = Buffer.from(value, 'base64url');
    if (token.length !== 65 || token[0] !== 1) return '';
    const expires = token.readUIntBE(11, 6);
    if (Math.floor(Date.now() / 1000) > expires || (token[1] & 128)) return '';
    return token.subarray(33).toString('hex');
}

function serve(r) {
    try {
        if (r.method === 'GET') {
            const header = r.headersIn['If-None-Match'];
            if (header && /^"[0-9a-f]{64}"$/.test(header)) {
                const before = ngx.shared.icon_control.get('active');
                if (before) {
                    const name = r.variables.icon_username.replace(/ +$/, '');
                    const hash = ngx.shared.icon_hashes.get(name);
                    const after = ngx.shared.icon_control.get('active');
                    if (before === after && Date.now() < Number(before.slice(37)) &&
                        header === '"' + hash + '"') {
                        r.variables.icon_session_id = measurementID(r);
                        r.headersOut['ETag'] = header;
                        r.return(304);
                        return;
                    }
                }
            }
        }
    } catch (_) {
        // An unavailable dictionary or unsupported validator uses the Go handler.
    }
    r.internalRedirect('@icon_reader');
}

export default {serve, status, publish};
