// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Mobile dashboard. Plain JavaScript on top of the REST API, so it runs in
// the iOS/Android WebViews and in any mobile browser without a build step.

(function () {
    'use strict';

    // ---- API access -----------------------------------------------------

    var meta = window.metadata || {};
    var apiKey = null;
    (function readKey() {
        var hash = new URLSearchParams(location.hash.slice(1));
        if (hash.get('apikey')) {
            apiKey = hash.get('apikey');
            try { sessionStorage.setItem('st-apikey', apiKey); } catch (e) { /* private mode */ }
            history.replaceState(null, '', location.pathname + location.search);
        } else {
            try { apiKey = sessionStorage.getItem('st-apikey'); } catch (e) { apiKey = null; }
        }
    })();

    function cookie(name) {
        var m = document.cookie.match('(?:^|; )' + name.replace(/[-.]/g, '\\$&') + '=([^;]*)');
        return m ? decodeURIComponent(m[1]) : '';
    }

    function headers(json) {
        var h = {};
        if (apiKey) {
            h['X-API-Key'] = apiKey;
        } else if (meta.deviceIDShort) {
            h['X-CSRF-Token-' + meta.deviceIDShort] = cookie('CSRF-Token-' + meta.deviceIDShort);
        }
        if (json) h['Content-Type'] = 'application/json';
        return h;
    }

    function api(method, path, query, body) {
        var url = '../rest/' + path;
        if (query) url += '?' + new URLSearchParams(query).toString();
        return fetch(url, {
            method: method,
            headers: headers(body !== undefined),
            body: body !== undefined ? JSON.stringify(body) : undefined,
            credentials: 'same-origin'
        }).then(function (r) {
            if (!r.ok) {
                return r.text().then(function (t) { throw new Error(t.trim() || r.statusText); });
            }
            var ct = r.headers.get('Content-Type') || '';
            return ct.indexOf('json') >= 0 ? r.json() : r.text();
        });
    }
    var get = function (p, q) { return api('GET', p, q); };
    var post = function (p, q, b) { return api('POST', p, q, b); };

    // ---- Formatting -----------------------------------------------------

    function esc(s) {
        return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
            return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
        });
    }

    function bytes(n) {
        if (!n) return '0 B';
        var u = ['B', 'KB', 'MB', 'GB', 'TB'], i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
        return (n / Math.pow(1024, i)).toFixed(i ? 1 : 0).replace(/\.0$/, '') + ' ' + u[i];
    }

    function count(n, one, many) { return n.toLocaleString() + ' ' + (n === 1 ? one : many); }

    function ago(iso) {
        var t = iso ? new Date(iso).getTime() : 0;
        if (!t || t < 86400000) return 'Never';
        var s = Math.round((Date.now() - t) / 1000);
        if (s < 45) return 'Just now';
        if (s < 3600) return Math.round(s / 60) + ' min ago';
        if (s < 86400) return Math.round(s / 3600) + ' h ago';
        if (s < 7 * 86400) return Math.round(s / 86400) + ' d ago';
        return new Date(t).toLocaleDateString();
    }

    function initials(name) {
        return (name || '?').split(/\s+/).map(function (w) { return w[0]; }).join('').slice(0, 2).toUpperCase();
    }

    var VIDEO = /\.(mov|mp4|m4v|3gp|avi|mkv)$/i;
    var ICON_PHOTO = '<svg viewBox="0 0 24 24"><rect x="3" y="5" width="18" height="14" rx="3"/><circle cx="9" cy="10" r="2"/><path d="m21 16-5-5-8 8"/></svg>';
    var ICON_VIDEO = '<svg viewBox="0 0 24 24"><rect x="3" y="6" width="13" height="12" rx="3"/><path d="m16 10 5-3v10l-5-3"/></svg>';
    var ICON_DIR = '<svg viewBox="0 0 24 24"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>';

    var STATE_LABEL = {
        'idle': ['Up to date', 'is-ok'],
        'scanning': ['Checking', 'is-busy'],
        'scan-waiting': ['Waiting to check', 'is-busy'],
        'sync-preparing': ['Preparing', 'is-busy'],
        'sync-waiting': ['Waiting to sync', 'is-busy'],
        'syncing': ['Syncing', 'is-busy'],
        'cleaning': ['Cleaning up', 'is-busy'],
        'error': ['Needs attention', 'is-warn'],
        'paused': ['Paused', ''],
        'unknown': ['Unknown', '']
    };

    // ---- State ----------------------------------------------------------

    var state = {
        config: null,
        myID: meta.deviceID || '',
        connections: {},
        folderStatus: {},
        folderStats: {},
        deviceStats: {},
        completion: {},      // deviceID -> folderID -> pct
        ignored: {},         // folderID -> [items]
        selected: new Set(), // names in the current ignored folder
        view: 'sync',
        ignoredFolder: null,
        version: ''
    };

    var $ = function (id) { return document.getElementById(id); };

    function folders() { return state.config ? state.config.folders : []; }
    function peers() {
        return state.config ? state.config.devices.filter(function (d) { return d.deviceID !== state.myID; }) : [];
    }
    function sharedWith(folder, deviceID) {
        return folder.devices.some(function (d) { return d.deviceID === deviceID; });
    }
    function deviceName(id) {
        var d = state.config && state.config.devices.find(function (x) { return x.deviceID === id; });
        return (d && d.name) || id.slice(0, 7);
    }

    // ---- Loading --------------------------------------------------------

    function refresh() {
        return Promise.all([
            get('config'),
            get('system/connections'),
            get('stats/folder'),
            get('stats/device'),
            state.myID ? null : get('system/status').then(function (s) { state.myID = s.myID; }),
            state.version ? null : get('system/version').then(function (v) { state.version = 'Keepsake · built on Syncthing ' + v.version; })
        ]).then(function (r) {
            state.config = r[0];
            state.connections = (r[1] && r[1].connections) || {};
            state.folderStats = r[2] || {};
            state.deviceStats = r[3] || {};

            var jobs = [];
            folders().forEach(function (f) {
                if (f.paused) {
                    state.folderStatus[f.id] = { state: 'paused' };
                    return;
                }
                jobs.push(get('db/status', { folder: f.id }).then(function (s) { state.folderStatus[f.id] = s; }));
                if (f.nonDestructiveDeletes) {
                    jobs.push(get('folder/ignoreddeletions', { folder: f.id }).then(function (r) {
                        state.ignored[f.id] = r.items || [];
                    }));
                } else {
                    state.ignored[f.id] = [];
                }
                peers().forEach(function (d) {
                    var c = state.connections[d.deviceID];
                    if (!c || !c.connected || !sharedWith(f, d.deviceID)) return;
                    jobs.push(get('db/completion', { folder: f.id, device: d.deviceID }).then(function (r) {
                        (state.completion[d.deviceID] = state.completion[d.deviceID] || {})[f.id] = r.completion;
                    }));
                });
            });
            return Promise.all(jobs.map(function (j) { return j.catch(function () { }); }));
        }).then(render).catch(function (err) {
            setPill('Offline', 'is-warn');
            $('hero-status').textContent = 'Can’t reach the sync service';
            $('hero-detail').textContent = err.message;
        });
    }

    var refreshTimer = null;
    function scheduleRefresh(delay) {
        clearTimeout(refreshTimer);
        refreshTimer = setTimeout(refresh, delay == null ? 400 : delay);
    }

    // Long-poll the event stream and refresh when something relevant
    // happens. Falls back to a periodic refresh if events fail.
    var lastEventID = 0;
    function pollEvents() {
        get('events', {
            since: lastEventID,
            timeout: 30,
            events: 'StateChanged,FolderSummary,FolderCompletion,DeviceConnected,DeviceDisconnected,DevicePaused,DeviceResumed,ConfigSaved,LocalIndexUpdated,FolderScanProgress'
        }).then(function (evs) {
            if (evs && evs.length) {
                lastEventID = evs[evs.length - 1].id;
                scheduleRefresh();
            }
            pollEvents();
        }).catch(function () {
            setTimeout(pollEvents, 5000);
        });
    }
    setInterval(function () { if (!document.hidden) refresh(); }, 30000);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) refresh(); });

    // ---- Rendering ------------------------------------------------------

    function setPill(text, cls) {
        var p = $('conn-pill');
        p.className = 'conn-pill ' + (cls || '');
        p.querySelector('.conn-label').textContent = text;
    }

    function render() {
        renderOverview();
        renderFolders();
        renderDevices();
        renderIgnored();
        renderSettings();
    }

    function renderOverview() {
        var global = 0, need = 0, needItems = 0, local = 0, busy = false, error = false, scanning = false;
        folders().forEach(function (f) {
            var s = state.folderStatus[f.id] || {};
            global += s.globalBytes || 0;
            need += s.needBytes || 0;
            needItems += s.needTotalItems || 0;
            local += s.localFiles || 0;
            if (s.state === 'error') error = true;
            else if (/sync|clean/.test(s.state || '')) busy = true;
            else if (/scan/.test(s.state || '')) scanning = true;
        });
        var connected = peers().filter(function (d) {
            var c = state.connections[d.deviceID];
            return c && c.connected;
        });
        var pct = global > 0 ? Math.floor(100 * (global - need) / global) : 100;
        if (need > 0 && pct === 100) pct = 99;

        var status, detail = '', cls = '';
        if (folders().length === 0) {
            status = 'No photo folder yet';
            detail = 'Open the app’s setup to choose what to sync.';
        } else if (error) {
            status = 'Needs attention'; cls = 'is-warn';
            detail = 'A folder reported a problem. See Folders below.';
        } else if (busy) {
            status = 'Syncing'; cls = 'is-busy';
            detail = count(needItems, 'item', 'items') + ' left · ' + bytes(need);
        } else if (scanning) {
            status = 'Checking for new photos'; cls = 'is-busy';
        } else if (connected.length === 0) {
            status = peers().length ? 'Waiting for your devices' : 'Add a device to start';
            detail = peers().length ? 'Sync resumes when another device is online.' : 'Pair your other phone in Devices.';
        } else {
            status = 'Up to date'; cls = 'is-ok';
            detail = 'Connected to ' + connected.map(function (d) { return d.name || d.deviceID.slice(0, 7); }).join(', ');
        }

        $('hero-status').textContent = status;
        $('hero-detail').textContent = detail;
        var ring = $('ring');
        ring.className = 'ring ' + cls;
        ring.setAttribute('aria-valuenow', String(pct));
        $('ring-pct').textContent = pct + '%';
        $('ring-value').style.strokeDashoffset = String(326.73 * (1 - pct / 100));

        var last = '';
        Object.keys(state.folderStats).forEach(function (id) {
            var at = state.folderStats[id].lastFile && state.folderStats[id].lastFile.at;
            if (at && at > last) last = at;
        });
        $('stat-last').textContent = ago(last);
        $('stat-local').textContent = local.toLocaleString();
        var ignoredTotal = Object.keys(state.ignored).reduce(function (n, k) { return n + state.ignored[k].length; }, 0);
        $('stat-ignored').textContent = ignoredTotal.toLocaleString();
        var badge = $('ignored-badge');
        badge.hidden = ignoredTotal === 0;
        badge.textContent = ignoredTotal > 99 ? '99+' : String(ignoredTotal);

        if (connected.length) setPill(connected.length === 1 ? 'Connected' : connected.length + ' connected', busy ? 'is-ok is-busy' : 'is-ok');
        else setPill(peers().length ? 'No devices online' : 'Not paired', 'is-warn');
    }

    function renderFolders() {
        $('folder-list').innerHTML = folders().map(function (f) {
            var s = state.folderStatus[f.id] || {};
            var st = STATE_LABEL[s.state] || STATE_LABEL.unknown;
            var pct = s.globalBytes ? Math.floor(100 * (s.globalBytes - s.needBytes) / s.globalBytes) : 100;
            var stats = state.folderStats[f.id] || {};
            var sub = count(s.localFiles || 0, 'photo', 'photos') + ' · last scan ' + ago(stats.lastScan).toLowerCase();
            if (s.error) sub = s.error;
            return '<li class="row">' +
                '<div class="avatar is-dir">' + ICON_DIR + '</div>' +
                '<div class="row-main"><span class="row-title">' + esc(f.label || f.id) + '</span>' +
                '<span class="row-sub">' + esc(sub) + '</span>' +
                (s.state === 'syncing' ? '<div class="bar is-busy"><span style="width:' + pct + '%"></span></div>' : '') +
                '</div>' +
                '<span class="chip ' + st[1] + '">' + st[0] + '</span>' +
                '</li>';
        }).join('');
    }

    function renderDevices() {
        var list = peers().map(function (d) {
            var c = state.connections[d.deviceID] || {};
            var ds = state.deviceStats[d.deviceID] || {};
            var comp = state.completion[d.deviceID] || {};
            var shared = folders().filter(function (f) { return sharedWith(f, d.deviceID); });
            var pcts = shared.map(function (f) { return comp[f.id]; }).filter(function (x) { return x != null; });
            var pct = pcts.length ? Math.floor(Math.min.apply(null, pcts)) : null;

            var dot = 'status-dot', sub;
            if (d.paused) { sub = 'Paused'; }
            else if (c.connected) {
                dot += pct != null && pct < 100 ? ' is-busy' : ' is-ok';
                var via = /relay/.test(c.type || '') ? 'via relay' : (c.isLocal ? 'on this network' : 'over the internet');
                sub = (pct == null || pct === 100 ? 'Up to date' : pct + '% synced') + ' · ' + via;
            } else {
                sub = 'Offline · last seen ' + ago(ds.lastSeen).toLowerCase();
            }
            return '<li class="row">' +
                '<div class="avatar">' + esc(initials(d.name || d.deviceID)) + '</div>' +
                '<div class="row-main"><span class="row-title">' + esc(d.name || d.deviceID.slice(0, 7)) + '</span>' +
                '<span class="row-sub">' + esc(sub) + '</span>' +
                (c.connected && pct != null ? '<div class="bar' + (pct < 100 ? ' is-busy' : '') + '"><span style="width:' + pct + '%"></span></div>' : '') +
                '</div>' +
                '<span class="' + dot + '" aria-hidden="true"></span>' +
                '<button class="btn btn-secondary btn-small" data-pause="' + esc(d.deviceID) + '" data-paused="' + (d.paused ? '1' : '') + '">' +
                (d.paused ? 'Resume' : 'Pause') + '</button>' +
                '</li>';
        });
        $('device-list').innerHTML = list.join('');

        if (state.myID && $('my-id').textContent !== state.myID) {
            $('my-id').textContent = state.myID;
            $('my-qr').src = '../qr/?text=' + encodeURIComponent(state.myID);
        }
    }

    function ndFolders() { return folders().filter(function (f) { return f.nonDestructiveDeletes; }); }

    function renderIgnored() {
        var nd = ndFolders();
        var sel = $('ignored-folder');
        if (!state.ignoredFolder || !nd.some(function (f) { return f.id === state.ignoredFolder; })) {
            state.ignoredFolder = nd.length ? nd[0].id : null;
            state.selected.clear();
        }
        sel.hidden = nd.length < 2;
        sel.innerHTML = nd.map(function (f) {
            return '<option value="' + esc(f.id) + '"' + (f.id === state.ignoredFolder ? ' selected' : '') + '>' + esc(f.label || f.id) + '</option>';
        }).join('');

        var q = $('ignored-search').value.trim().toLowerCase();
        var items = (state.ignored[state.ignoredFolder] || []).filter(function (it) {
            return !q || it.name.toLowerCase().indexOf(q) >= 0;
        });
        // Drop selections that no longer exist
        var names = new Set(items.map(function (it) { return it.name; }));
        state.selected.forEach(function (n) { if (!names.has(n)) state.selected.delete(n); });

        $('ignored-list').innerHTML = items.map(function (it) {
            var base = it.name.split('/').pop();
            var dir = it.name.slice(0, it.name.length - base.length).replace(/\/$/, '');
            var isDir = it.type === 'directory';
            var icon = isDir ? ICON_DIR : (VIDEO.test(base) ? ICON_VIDEO : ICON_PHOTO);
            var avatarCls = isDir ? 'avatar is-dir' : (VIDEO.test(base) ? 'avatar is-video' : 'avatar');
            var checked = state.selected.has(it.name);
            var where = it.availableOn && it.availableOn.length
                ? it.availableOn.map(function (id) { return '<span class="chip is-ok">On ' + esc(deviceName(id)) + '</span>'; }).join('')
                : '<span class="chip is-warn">Not on any device</span>';
            return '<li class="row' + (checked ? ' is-selected' : '') + '">' +
                '<label class="row-hit">' +
                '<input type="checkbox" data-name="' + esc(it.name) + '"' + (checked ? ' checked' : '') + ' aria-label="Select ' + esc(base) + '">' +
                '<div class="' + avatarCls + '">' + icon + '</div>' +
                '<div class="row-main"><span class="row-title">' + esc(base) + '</span>' +
                '<span class="row-sub">' + esc((dir ? dir + ' · ' : '') + (isDir ? 'Album' : bytes(it.size)) + ' · deleted ' + ago(it.ignoredAt).toLowerCase()) + '</span>' +
                '<div class="device-chips">' + where + '</div></div>' +
                '</label></li>';
        }).join('');

        var total = (state.ignored[state.ignoredFolder] || []).length;
        $('ignored-empty').hidden = items.length > 0;
        $('select-all-row').hidden = items.length === 0;
        $('ignored-count').textContent = count(total, 'item', 'items');
        $('select-all').checked = items.length > 0 && items.every(function (it) { return state.selected.has(it.name); });
        updateActionBar();
    }

    function updateActionBar() {
        var n = state.selected.size;
        $('action-bar').hidden = state.view !== 'ignored' || n === 0;
        $('action-count').textContent = n + ' selected';
    }

    function renderSettings() {
        $('settings-folders').innerHTML = folders().filter(function (f) {
            return f.type === 'sendreceive' || f.type === 'sendonly';
        }).map(function (f) {
            return '<li class="row"><div class="row-main">' +
                '<span class="row-title">' + esc(f.label || f.id) + '</span>' +
                '<span class="row-sub">Keep photos on other devices when I delete them here</span></div>' +
                '<label class="switch"><input type="checkbox" data-nd="' + esc(f.id) + '"' + (f.nonDestructiveDeletes ? ' checked' : '') + '>' +
                '<span aria-hidden="true"></span><span class="visually-hidden">Keep ' + esc(f.label || f.id) + ' photos on other devices</span></label></li>';
        }).join('');
        var all = peers();
        $('toggle-pause-all').checked = all.length > 0 && all.every(function (d) { return d.paused; });
        $('version').textContent = state.version;
    }

    // ---- Actions --------------------------------------------------------

    var toastTimer = null;
    function toast(msg) {
        var t = $('toast');
        t.textContent = msg;
        t.hidden = false;
        clearTimeout(toastTimer);
        toastTimer = setTimeout(function () { t.hidden = true; }, 3200);
    }

    function fail(err) { toast(err && err.message ? err.message : 'Something went wrong'); }

    function showView(view) {
        state.view = view;
        document.querySelectorAll('.view').forEach(function (v) { v.hidden = v.id !== 'view-' + view; });
        document.querySelectorAll('.tab').forEach(function (t) {
            t.setAttribute('aria-selected', String(t.dataset.view === view));
        });
        $('view-title').textContent = { sync: 'Keepsake', devices: 'Devices', ignored: 'Ignored', settings: 'Settings' }[view];
        updateActionBar();
        window.scrollTo(0, 0);
    }

    document.querySelectorAll('.tab').forEach(function (t) {
        t.addEventListener('click', function () { showView(t.dataset.view); });
    });

    $('btn-sync-now').addEventListener('click', function (e) {
        var btn = e.currentTarget;
        btn.disabled = true;
        Promise.all(folders().map(function (f) { return post('db/scan', { folder: f.id }); }))
            .then(function () { toast('Checking for new photos'); scheduleRefresh(0); })
            .catch(fail)
            .then(function () { btn.disabled = false; });
    });

    $('device-list').addEventListener('click', function (e) {
        var btn = e.target.closest('[data-pause]');
        if (!btn) return;
        btn.disabled = true;
        post(btn.dataset.paused ? 'system/resume' : 'system/pause', { device: btn.dataset.pause })
            .then(function () { scheduleRefresh(0); }).catch(fail);
    });

    $('btn-copy-id').addEventListener('click', function () {
        if (navigator.clipboard) {
            navigator.clipboard.writeText(state.myID).then(function () { toast('Device ID copied'); }, fail);
        }
    });

    $('add-device-form').addEventListener('submit', function (e) {
        e.preventDefault();
        var form = e.currentTarget;
        var id = form.deviceID.value.trim().toUpperCase().replace(/\s+/g, '');
        var name = form.name.value.trim();
        var btn = form.querySelector('button');
        btn.disabled = true;
        api('PUT', 'config/devices/' + encodeURIComponent(id), null, { deviceID: id, name: name })
            .then(function () {
                // Share every photo folder with the new device.
                return Promise.all(ndFolders().filter(function (f) { return !sharedWith(f, id); }).map(function (f) {
                    var devs = f.devices.concat([{ deviceID: id }]);
                    return api('PATCH', 'config/folders/' + encodeURIComponent(f.id), null, { devices: devs });
                }));
            })
            .then(function () { form.reset(); toast('Device added. Accept it on the other phone.'); scheduleRefresh(0); })
            .catch(fail)
            .then(function () { btn.disabled = false; });
    });

    $('ignored-folder').addEventListener('change', function (e) {
        state.ignoredFolder = e.target.value;
        state.selected.clear();
        renderIgnored();
    });
    $('ignored-search').addEventListener('input', renderIgnored);

    $('ignored-list').addEventListener('change', function (e) {
        var cb = e.target;
        if (!cb.dataset || cb.dataset.name == null) return;
        if (cb.checked) state.selected.add(cb.dataset.name); else state.selected.delete(cb.dataset.name);
        cb.closest('.row').classList.toggle('is-selected', cb.checked);
        $('select-all').checked = false;
        updateActionBar();
    });

    $('select-all').addEventListener('change', function (e) {
        var on = e.target.checked;
        document.querySelectorAll('#ignored-list input[data-name]').forEach(function (cb) {
            cb.checked = on;
            if (on) state.selected.add(cb.dataset.name); else state.selected.delete(cb.dataset.name);
            cb.closest('.row').classList.toggle('is-selected', on);
        });
        updateActionBar();
    });

    function runOnSelection(action, done) {
        var names = Array.from(state.selected);
        var folder = state.ignoredFolder;
        $('btn-restore').disabled = $('btn-delete-everywhere').disabled = true;
        post('folder/ignoreddeletions/' + action, { folder: folder }, { names: names })
            .then(function () {
                // Optimistic update; the event stream refreshes the rest.
                state.ignored[folder] = (state.ignored[folder] || []).filter(function (it) {
                    return !names.some(function (n) { return it.name === n || it.name.indexOf(n + '/') === 0; });
                });
                state.selected.clear();
                renderIgnored();
                renderOverview();
                toast(done(names.length));
                scheduleRefresh(1000);
            })
            .catch(fail)
            .then(function () { $('btn-restore').disabled = $('btn-delete-everywhere').disabled = false; });
    }

    $('btn-restore').addEventListener('click', function () {
        runOnSelection('restore', function (n) { return 'Restoring ' + count(n, 'item', 'items') + ' to this phone'; });
    });

    $('btn-delete-everywhere').addEventListener('click', function () {
        var n = state.selected.size;
        $('confirm-title').textContent = 'Delete ' + count(n, 'item', 'items') + ' everywhere?';
        $('confirm-body').textContent = 'They will be removed from all your other devices too. This can’t be undone from this phone.';
        var dlg = $('confirm-dialog');
        dlg.returnValue = '';
        dlg.addEventListener('close', function onClose() {
            dlg.removeEventListener('close', onClose);
            if (dlg.returnValue === 'ok') {
                runOnSelection('propagate', function (k) { return 'Deleted ' + count(k, 'item', 'items') + ' everywhere'; });
            }
        });
        if (dlg.showModal) dlg.showModal();
        else if (window.confirm($('confirm-title').textContent)) {
            runOnSelection('propagate', function (k) { return 'Deleted ' + count(k, 'item', 'items') + ' everywhere'; });
        }
    });

    $('settings-folders').addEventListener('change', function (e) {
        var id = e.target.dataset && e.target.dataset.nd;
        if (!id) return;
        api('PATCH', 'config/folders/' + encodeURIComponent(id), null, { nonDestructiveDeletes: e.target.checked })
            .then(function () {
                toast(e.target.checked ? 'Deleting here will keep photos on other devices' : 'Deletions will now sync to other devices');
                scheduleRefresh(0);
            })
            .catch(function (err) { e.target.checked = !e.target.checked; fail(err); });
    });

    $('toggle-pause-all').addEventListener('change', function (e) {
        post(e.target.checked ? 'system/pause' : 'system/resume')
            .then(function () { scheduleRefresh(0); })
            .catch(function (err) { e.target.checked = !e.target.checked; fail(err); });
    });

    // ---- Start ----------------------------------------------------------

    refresh().then(pollEvents);
})();
