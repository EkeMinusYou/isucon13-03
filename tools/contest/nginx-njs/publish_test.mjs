import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';

// Observe dictionary visibility at each write, including publication failures.
const source = readFileSync(new URL('../../../nginx/njs/icon.js', import.meta.url), 'utf8');
const load = new Function('ngx', source.replace('export default {status, publish};', 'return {status, publish};'));
const oldID = '11111111-1111-1111-1111-111111111111';
const newID = '22222222-2222-2222-2222-222222222222';

function fixture() {
    const observations = [];
    let failKey;
    class Dict extends Map {
        set(key, value) {
            if (key === failKey) {
                failKey = undefined;
                throw new Error('injected dictionary write failure');
            }
            super.set(key, value);
            if (observe) record();
        }
        delete(key) { const result = super.delete(key); if (observe) record(); return result; }
        clear() { super.clear(); if (observe) record(); }
        add(key, value) { if (this.has(key)) return false; this.set(key, value); return true; }
        size() { return super.size; }
        freeSpace() { return 65536; }
    }
    let observe = false;
    const control = new Dict();
    const hashes = new Dict();
    const expires = Date.now() + 400;
    const old = {publish_id: oldID, epoch: 'owner', revision: 7, dirty: false, valid_until_ms: expires};
    control.set('state', JSON.stringify(old));
    control.set('active', oldID + ':' + expires);
    hashes.set('alpha', 'a'.repeat(64));
    function record() {
        observations.push({active: control.get('active'), hash: hashes.get('alpha'),
            state: JSON.parse(control.get('state'))});
    }
    observe = true;
    const handlers = load({shared: {icon_control: control, icon_hashes: hashes}});
    const payload = {mode: 'delta', base_publish_id: oldID, publish_id: newID,
        epoch: 'owner', base_revision: 7, revision: 7, valid_until_ms: Date.now() + 700, changes: []};
    function publish(change = {}) {
        let code;
        handlers.publish({method: 'POST', requestText: JSON.stringify({...payload, ...change}),
            headersOut: {}, return(status) { code = status; }});
        return code;
    }
    return {control, hashes, observations, payload, publish, fail(key) { failKey = key; }};
}

{
    const f = fixture();
    assert.equal(f.publish(), 200);
    assert.ok(f.observations.every(o => o.active && o.hash === 'a'.repeat(64)),
        'an unchanged renewal must remain readable at every dictionary write');
    const pending = f.observations.find(o => o.state.publish_id === newID && o.active.startsWith(oldID));
    assert.ok(pending, 'the old publication must retain its own expiration until lease replacement');
    assert.notEqual(pending.active, oldID + ':' + f.payload.valid_until_ms);
    assert.equal(f.control.get('active'), newID + ':' + f.payload.valid_until_ms);
    assert.equal(f.publish(), 200, 'a lost ACK can retry the same publication');
}
{
    const f = fixture();
    assert.equal(f.publish({revision: 8, changes: [{name: 'alpha', hash: 'b'.repeat(64)}]}), 200);
    assert.ok(f.observations.filter(o => o.hash === 'b'.repeat(64))
        .every(o => !o.active || o.active.startsWith(newID)), 'new hashes must not be visible under the old lease');
    assert.ok(f.observations.some(o => !o.active && o.state.dirty));
}
{
    const f = fixture();
    assert.equal(f.publish({revision: 8}), 200);
    assert.ok(f.observations.some(o => !o.active), 'a revision change must use guarded publication');
}
for (const change of [{epoch: 'other'}, {base_revision: 6}, {valid_until_ms: Date.now() - 1},
    {valid_until_ms: Date.now() + 2000}]) {
    const f = fixture();
    assert.equal(f.publish(change), 409);
    assert.ok(f.control.get('active').startsWith(oldID), 'a rejected renewal must preserve the prior lease');
}
{
    const f = fixture();
    const state = JSON.parse(f.control.get('state'));
    f.control.set('state', JSON.stringify({...state, dirty: true}));
    f.control.delete('active');
    assert.equal(f.publish(), 409);
    assert.equal(f.control.has('active'), false, 'a dirty publication cannot renew');
}
for (const key of ['state', 'active']) {
    const f = fixture();
    f.fail(key);
    assert.equal(f.publish(), 503);
    assert.equal(f.control.has('active'), false, 'a failed renewal must close direct reads');
    assert.equal(JSON.parse(f.control.get('state')).dirty, true);
    assert.equal(f.control.has('writer'), false);
}
console.log('PASS unchanged renewal visibility, hash replacement, revision/epoch/lease guards, retry and write failures');
