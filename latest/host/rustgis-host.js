// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0
//
// Data Store panel over the RustGIS canvas. Contract: docs/architecture.md, section 6.
(function () {
  "use strict";

  const PLACEHOLDER = "https://rgis-host.invalid/fs/";
  const MAX_OPEN = 16;
  const MAX_URL = 6000;
  const MAX_CHUNK = 8388608;
  const MIN_CHUNK = 262144;
  const RETRY_WAITS = [1000, 2000, 4000, 8000, 16000];
  const EXPIRED = "Session expired; reload the page";
  const NO_STORE = "The Data Store is not mounted in this analysis. In the Discovery Environment, launch the app again with “Mount data store in analysis” turned on.";
  const DOCK_KEY = "rgis-host.dock";
  const P = "rgis-host-";

  class HostError extends Error {
    constructor(code, message, status, offset) {
      super(message);
      this.name = "HostError";
      this.code = code;
      this.status = status || 0;
      this.offset = offset;
    }
  }

  // ---------------------------------------------------------------- URLs

  const encPath = (p) => p.split("/").map(encodeURIComponent).join("/");
  const fileUrl = (p) => location.origin + "/fs" + encPath(p);
  const projectUrl = (p) => location.origin + "/api/v1/project" + encPath(p);

  function keep() {
    return location.search
      .replace(/^\?/, "")
      .split("&")
      .filter((s) => ["webgl", "gpu", "lang"].includes(s.split("=")[0]))
      .map((s) => "&" + s)
      .join("");
  }

  // Upstream picks the WebGL backend when "webgl" occurs anywhere in the query string (v0.6.0
  // web.rs start()), so the word must not survive in an address; %77 decodes back to "w".
  const queryValue = (url) => encodeURIComponent(url).replace(/webgl/g, "%77ebgl");

  function launchUrl(paths) {
    const list = Array.from(paths || []);
    if (list.length < 1 || list.length > MAX_OPEN) {
      throw new RangeError("launchUrl takes 1 to " + MAX_OPEN + " paths");
    }
    const url = location.origin + "/?" + list.map((p) => "data=" + queryValue(fileUrl(p))).join("&") + keep();
    if (url.length > MAX_URL) throw new RangeError("launch URL is longer than " + MAX_URL + " characters");
    return url;
  }

  const projectLaunchUrl = (p) => location.origin + "/?project=" + queryValue(projectUrl(p)) + keep();
  const packageLaunchUrl = (p) => location.origin + "/?project=" + queryValue(fileUrl(p)) + keep();

  // ---------------------------------------------------------------- API

  let expired = false;

  async function request(method, route, params, init) {
    if (expired) throw new HostError("session_expired", EXPIRED);
    const headers = { "X-RGIS-Host": "1" };
    if (method === "PUT") headers["Content-Type"] = "application/octet-stream";
    if (method === "POST") headers["Content-Type"] = "application/json";
    const query = params ? "?" + new URLSearchParams(params) : "";
    let res;
    try {
      res = await fetch("/api/v1/" + route + query, {
        method,
        headers,
        body: init && init.body,
        signal: init && init.signal,
        credentials: "same-origin",
        cache: "no-store",
        redirect: "manual",
      });
    } catch (e) {
      if (e && e.name === "AbortError") throw new HostError("aborted", "Cancelled");
      throw new HostError("network", "Network error");
    }
    if (res.type === "opaqueredirect") {
      expire();
      throw new HostError("session_expired", EXPIRED);
    }
    let body = null;
    if (method !== "HEAD" && res.status !== 204 && /json/i.test(res.headers.get("Content-Type") || "")) {
      body = await res.json().catch(() => null);
    }
    if (res.ok) {
      if (method !== "HEAD" && res.status !== 204 && (body === null || typeof body !== "object")) {
        throw new HostError("http", "Unexpected response (HTTP " + res.status + ")", res.status);
      }
      return { status: res.status, body, headers: res.headers };
    }
    const err = body && body.error;
    if (err && typeof err.code === "string") {
      const offset = typeof body.offset === "number" ? body.offset : undefined;
      throw new HostError(err.code, describe(err.code, err.message), res.status, offset);
    }
    throw new HostError("http", "Request failed (HTTP " + res.status + ")", res.status);
  }

  function describe(code, message) {
    const known = {
      bad_path: "That name is not allowed (no leading dot, slash, backslash or control characters)",
      not_found: "Not found",
      read_only: "This location is read-only",
      forbidden: "Permission denied",
      exists: "Something with that name already exists",
      no_space: "The Data Store is out of space or over quota",
      store_error: "The Data Store refused or failed the operation (permission, quota or connection)",
      forbidden_origin: "The request was refused: this page is not on the analysis's own address",
    };
    return known[code] || (typeof message === "string" && message ? message : code);
  }

  function expire() {
    if (expired) return;
    expired = true;
    stopUploads(EXPIRED);
    setStatus(EXPIRED, "error");
  }

  // ---------------------------------------------------------------- session

  let resolveReady;
  const ready = new Promise((resolve) => (resolveReady = resolve));
  let sessionLoad = null;

  // force refetches: roots can appear after start (exists is evaluated per request).
  function ensureSession(force) {
    if (host.session && !force) return Promise.resolve(host.session);
    if (!sessionLoad) {
      sessionLoad = request("GET", "session")
        .then((res) => {
          const s = res.body;
          if (!Array.isArray(s.roots) || !s.limits) throw new HostError("http", "Unexpected session response");
          host.session = s;
          if (s.origin !== location.origin) {
            console.warn("rustgis-host: server origin " + s.origin + " differs from " + location.origin + "; saved projects may not reopen");
          }
          if (ui) renderSession();
          resolveReady();
          return s;
        })
        .finally(() => (sessionLoad = null));
    }
    return sessionLoad;
  }

  // ---------------------------------------------------------------- state

  const state = {
    open: false,
    path: "", // "" is the top level (root list)
    writable: false,
    entries: [],
    cursor: "",
    selected: new Set(),
    token: 0,
    message: null, // {text, kind, url}
    hint: "",
    lastFocus: null,
  };
  const uploads = [];
  let pumping = false;
  let ui = null;

  const canWrite = () => !!(state.path && state.writable && host.session && !host.session.readOnly);

  // ---------------------------------------------------------------- DOM helpers

  function el(tag, cls, attrs, kids) {
    const node = document.createElement(tag);
    if (cls) node.className = cls.split(" ").map((c) => P + c).join(" ");
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k === "text") node.textContent = v;
      else if (k === "id") node.id = P + v;
      else node.setAttribute(k, v);
    }
    for (const kid of kids || []) if (kid) node.append(kid);
    return node;
  }

  const ICONS = {
    dir: "M2 4.5A1.5 1.5 0 0 1 3.5 3h3l1.5 1.5h4.5A1.5 1.5 0 0 1 14 6v5.5a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 2 11.5z",
    file: "M4.5 1.5h4.6l3.4 3.4v8.6a1 1 0 0 1-1 1h-7a1 1 0 0 1-1-1v-11a1 1 0 0 1 1-1zM9 1.8V5h3.2",
    close: "M3.5 3.5l9 9M12.5 3.5l-9 9",
    upload: "M8 11V3M4.8 6 8 2.8 11.2 6M3 11.5v1.2a.8.8 0 0 0 .8.8h8.4a.8.8 0 0 0 .8-.8v-1.2",
    refresh: "M13 8a5 5 0 1 1-1.5-3.6M13 2.5V5h-2.5",
  };

  function icon(name) {
    const ns = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("class", P + "icon " + P + "icon-" + name);
    const path = document.createElementNS(ns, "path");
    path.setAttribute("d", ICONS[name]);
    svg.append(path);
    return svg;
  }

  function fmtSize(n) {
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (n >= 1024 && i < units.length - 1) {
      n /= 1024;
      i++;
    }
    return (i && n < 10 ? n.toFixed(1) : Math.round(n)) + " " + units[i];
  }

  function fmtTime(iso) {
    const d = new Date(iso);
    return isNaN(d) ? "" : d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
  }

  const baseName = (p) => p.slice(p.lastIndexOf("/") + 1);

  // ---------------------------------------------------------------- build

  function build() {
    const u = {};
    u.toggle = el("button", "toggle", { id: "toggle", type: "button", "aria-expanded": "false", "aria-controls": P + "panel", title: "Data Store (drag to move)" }, [
      el("span", "toggle-dot", { "aria-hidden": "true" }),
      el("span", "toggle-label", { text: "Data Store" }),
    ]);
    u.backdrop = el("div", "", { id: "backdrop", hidden: "" });

    u.user = el("span", "user");
    u.badge = el("span", "badge", { text: "Read-only", hidden: "" });
    u.refresh = el("button", "icon-btn", { id: "refresh", type: "button", "aria-label": "Reload", title: "Reload" }, [icon("refresh")]);
    u.close = el("button", "icon-btn", { id: "close", type: "button", "aria-label": "Close Data Store panel", title: "Close" }, [icon("close")]);
    const header = el("header", "header", null, [
      el("h2", "title", { id: "title", text: "Data Store" }),
      u.user,
      u.badge,
      u.refresh,
      u.close,
    ]);

    u.roots = el("nav", "", { id: "roots", "aria-label": "Locations" });
    u.crumbs = el("nav", "", { id: "crumbs", "aria-label": "Current folder" });
    u.filter = el("input", "input", { id: "filter", type: "text", placeholder: "Filter by name", "aria-label": "Filter by name", autocomplete: "off", spellcheck: "false" });

    u.list = el("ul", "", { id: "list", tabindex: "-1", "aria-label": "Files and folders" });
    u.empty = el("p", "empty");
    u.more = el("button", "btn", { id: "more", type: "button", text: "Load more" });
    u.body = el("div", "body", null, [u.list, u.empty]);

    u.mkdirName = el("input", "input", { id: "mkdir-name", type: "text", placeholder: "New folder name", "aria-label": "New folder name", autocomplete: "off", spellcheck: "false", maxlength: "255" });
    u.mkdir = el("button", "btn", { id: "mkdir", type: "button", text: "New folder" });
    u.upload = el("button", "btn", { id: "upload", type: "button" }, [icon("upload"), el("span", "", { text: "Upload" })]);
    u.uploadInput = el("input", "", { id: "upload-input", type: "file", multiple: "", hidden: "", tabindex: "-1" });
    u.tools = el("div", "tools", null, [u.mkdirName, u.mkdir, u.upload, u.uploadInput]);

    u.uploads = el("ul", "", { id: "uploads", "aria-label": "Uploads", hidden: "" });

    u.count = el("span", "count");
    u.open = el("a", "btn primary", { id: "open", text: "Open in new map" });
    u.openProject = el("a", "btn", { id: "open-project", text: "Open project" });
    u.copy = el("button", "btn", { id: "copy", type: "button", text: "Copy URL", title: "The URL works in this analysis only (Add Data › From URL)" });
    const actions = el("div", "actions", null, [u.open, u.openProject, u.copy, u.count]);
    const note = el("p", "note", { text: "Opening replaces the current map. Ctrl- or ⌘-click opens a new tab instead." });
    u.status = el("div", "", { id: "status", role: "status" });

    u.drop = el("div", "drop", { "aria-hidden": "true", text: "Drop files to upload here" });
    u.panel = el("section", "", { id: "panel", role: "dialog", "aria-modal": "true", "aria-labelledby": P + "title", tabindex: "-1", hidden: "" }, [
      header,
      u.roots,
      el("div", "nav", null, [u.crumbs, u.filter]),
      u.body,
      u.tools,
      u.uploads,
      el("footer", "footer", null, [actions, note, u.status]),
      u.drop,
    ]);
    u.root = el("div", detectBridge() ? "bridge" : "launch", { id: "root" }, [u.toggle, u.backdrop, u.panel]);
    ui = u;

    u.backdrop.addEventListener("click", close);
    u.close.addEventListener("click", close);
    u.refresh.addEventListener("click", () =>
      ensureSession(true)
        .then(() => state.path && load(state.path, true))
        .catch(showError)
    );
    u.filter.addEventListener("input", applyFilter);
    u.more.addEventListener("click", () => more().catch(showError));
    u.mkdir.addEventListener("click", submitMkdir);
    u.upload.addEventListener("click", () => u.uploadInput.click());
    u.uploadInput.addEventListener("change", () => {
      const files = Array.from(u.uploadInput.files);
      u.uploadInput.value = "";
      if (files.length && canWrite()) upload(files, state.path).catch(() => {});
    });
    u.copy.addEventListener("click", copyUrl);

    // eframe listens for clipboard events on document; drops outside the panel must not navigate
    // the tab to the dropped file.
    for (const type of ["copy", "cut", "paste", "dragover", "drop"]) {
      u.root.addEventListener(type, (e) => {
        e.stopPropagation();
        if (type === "dragover" || type === "drop") e.preventDefault();
        if (type === "dragover") onDragOver(e);
        if (type === "drop") onDrop(e);
      });
    }
    u.root.addEventListener("focusin", (e) => (state.lastFocus = e.target));
    document.addEventListener("focusin", (e) => {
      if (state.open && !u.root.contains(e.target)) refocus();
    });

    makeDockable(u.toggle);
    document.body.append(u.root);
    if (host.session) renderSession();
    renderCrumbs();
    renderEmpty();
    updateTools();
    updateActions();
  }

  // The toggle is a tab on the right edge. Upstream's layout there is not ours to know, so the
  // user can drag it up or down; the position is remembered.
  function makeDockable(btn) {
    const setDock = (pct) => ui.root.style.setProperty("--rgis-host-dock", String(Math.min(100, Math.max(0, pct))));
    try {
      const saved = parseFloat(localStorage.getItem(DOCK_KEY));
      if (saved >= 0 && saved <= 100) setDock(saved);
    } catch (e) {
      // storage disabled
    }
    let start = null;
    let moved = false;
    btn.addEventListener("pointerdown", (e) => {
      if (e.button !== 0) return;
      start = { y: e.clientY, top: btn.getBoundingClientRect().top };
      moved = false;
      try {
        btn.setPointerCapture(e.pointerId);
      } catch (err) {
        // capture is a convenience
      }
    });
    btn.addEventListener("pointermove", (e) => {
      if (!start) return;
      const dy = e.clientY - start.y;
      if (!moved && Math.abs(dy) < 5) return;
      moved = true;
      setDock(((start.top + dy) / window.innerHeight) * 100);
    });
    const end = () => {
      if (start && moved) {
        try {
          localStorage.setItem(DOCK_KEY, ui.root.style.getPropertyValue("--rgis-host-dock"));
        } catch (e) {
          // storage disabled
        }
        // The press moved focus to the tab; hand the keyboard back to the map.
        const canvas = document.getElementById("rustgis_canvas");
        if (canvas && !state.open) canvas.focus({ preventScroll: true });
      }
      start = null;
      // The click that ends a drag fires before this timer; a later keyboard click must not be eaten.
      setTimeout(() => (moved = false), 0);
    };
    btn.addEventListener("pointerup", end);
    btn.addEventListener("pointercancel", end);
    btn.addEventListener("click", () => {
      if (!moved) toggle();
    });
  }

  // ---------------------------------------------------------------- panel visibility and focus

  function open() {
    if (!ui || state.open) return;
    state.open = true;
    ui.panel.hidden = false;
    ui.backdrop.hidden = false;
    ui.root.classList.add(P + "open");
    ui.toggle.setAttribute("aria-expanded", "true");
    ui.filter.focus({ preventScroll: true });
    if (!host.session && !expired) ensureSession().catch(showError);
  }

  function close() {
    if (!ui || !state.open) return;
    state.open = false;
    ui.panel.hidden = true;
    ui.backdrop.hidden = true;
    ui.root.classList.remove(P + "open");
    ui.toggle.setAttribute("aria-expanded", "false");
    const canvas = document.getElementById("rustgis_canvas");
    if (canvas) canvas.focus({ preventScroll: true });
  }

  const toggle = () => (state.open ? close() : open());

  const focusables = () =>
    Array.from(ui.panel.querySelectorAll("a[href], button:not([disabled]), input:not([disabled])")).filter((n) => n.getClientRects().length);

  function refocus() {
    const last = state.lastFocus;
    if (last && last.isConnected && ui.root.contains(last) && last.getClientRects().length && !last.disabled) last.focus({ preventScroll: true });
    else ui.panel.focus({ preventScroll: true });
  }

  // Re-rendering can remove the focused element; focus must stay inside the modal panel so that
  // Escape and Tab keep reaching the overlay's key handler.
  function keepFocus() {
    if (state.open && !ui.root.contains(document.activeElement)) ui.list.focus({ preventScroll: true });
  }

  // Registered at script evaluation, so it runs before upstream's own window-capture keydown
  // listener, which calls preventDefault() on Ctrl/Cmd shortcuts such as Ctrl+A.
  window.addEventListener(
    "keydown",
    (e) => {
      if (!ui || !(e.target instanceof Node)) return;
      if (!ui.root.contains(e.target)) {
        // Hiding or removing the focused control drops focus to <body>. The panel is modal, so
        // Escape and Tab there still belong to it; every other key is left alone.
        const lost = state.open && (e.target === document.body || e.target === document.documentElement);
        if (!lost || (e.key !== "Escape" && e.key !== "Tab")) return;
      }
      e.stopImmediatePropagation();
      if (e.key === "Escape") {
        if (state.open) {
          e.preventDefault();
          close();
        }
      } else if (e.key === "Tab" && state.open) {
        const items = focusables();
        if (!items.length) return;
        const first = items[0];
        const last = items[items.length - 1];
        const at = document.activeElement;
        if (!ui.panel.contains(at) || at === ui.panel || at === ui.list) {
          e.preventDefault();
          (e.shiftKey ? last : first).focus();
        } else if (e.shiftKey && at === first) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && at === last) {
          e.preventDefault();
          first.focus();
        }
      } else if (e.key === "Enter" && e.target === ui.mkdirName && !e.isComposing) {
        e.preventDefault();
        submitMkdir();
      }
    },
    true
  );

  // ---------------------------------------------------------------- status line

  function setStatus(text, kind, url) {
    state.message = text ? { text, kind: kind || "", url } : null;
    renderStatus();
  }

  function showError(e) {
    if (e && e.code === "aborted") return;
    setStatus(e && e.message ? e.message : "Something went wrong", "error");
  }

  function renderStatus() {
    if (!ui) return;
    const m = state.message || { text: state.hint, kind: "" };
    ui.status.className = m.kind ? P + "status-" + m.kind : "";
    ui.status.textContent = m.text || "";
    if (m.url) {
      const box = el("input", "input", { type: "text", readonly: "", "aria-label": "File URL" });
      box.value = m.url;
      ui.status.append(box);
      box.focus();
      box.select();
    }
  }

  // ---------------------------------------------------------------- session and roots

  function renderSession() {
    const s = host.session;
    ui.user.textContent = s.user || "";
    ui.badge.hidden = !s.readOnly;
    ui.roots.replaceChildren(
      ...s.roots.map((r) => {
        const b = el("button", "root", { type: "button", "data-path": r.path, text: r.label });
        if (!r.exists) {
          b.disabled = true;
          b.title = "Not available in this analysis";
        } else {
          b.title = r.path + (r.writable ? "" : " (read-only)");
          b.addEventListener("click", () => navigate(r.path).catch(() => {}));
        }
        if (!r.writable) b.classList.add(P + "root-ro");
        return b;
      })
    );
    markRoot();
    renderEmpty();
    updateTools();
  }

  function rootOf(path) {
    let best = null;
    for (const r of (host.session && host.session.roots) || []) {
      if ((path === r.path || path.startsWith(r.path + "/")) && (!best || r.path.length > best.path.length)) best = r;
    }
    return best;
  }

  function markRoot() {
    const cur = state.path && rootOf(state.path);
    for (const b of ui.roots.children) {
      if (cur && b.dataset.path === cur.path) b.setAttribute("aria-current", "true");
      else b.removeAttribute("aria-current");
    }
  }

  function renderCrumbs() {
    const kids = [];
    const root = state.path && rootOf(state.path);
    if (root) {
      const rest = state.path.slice(root.path.length).split("/").filter(Boolean);
      const steps = [{ label: root.label, path: root.path }];
      rest.forEach((seg, i) => steps.push({ label: seg, path: root.path + "/" + rest.slice(0, i + 1).join("/") }));
      steps.forEach((s, i) => {
        if (i) kids.push(el("span", "crumb-sep", { "aria-hidden": "true", text: "/" }));
        const b = el("button", "crumb", { type: "button", text: s.label, title: s.path });
        if (i === steps.length - 1) b.setAttribute("aria-current", "location");
        b.addEventListener("click", () => navigate(s.path).catch(() => {}));
        kids.push(b);
      });
    } else if (state.path) {
      kids.push(el("span", "crumb-text", { text: state.path }));
    } else {
      kids.push(el("span", "crumb-text", { text: "Choose a location" }));
    }
    ui.crumbs.replaceChildren(...kids);
  }

  // ---------------------------------------------------------------- listing

  async function load(path, keepSelection) {
    const token = ++state.token;
    ui.list.setAttribute("aria-busy", "true");
    let res;
    try {
      res = await request("GET", "list", { path });
    } catch (e) {
      if (token === state.token) {
        ui.list.removeAttribute("aria-busy");
        showError(e);
      }
      throw e;
    }
    if (token !== state.token) return;
    const b = res.body;
    const entries = Array.isArray(b.entries) ? b.entries : [];
    const names = new Set(entries.map((e) => path + "/" + e.name));
    state.selected = new Set(keepSelection && path === state.path ? Array.from(state.selected).filter((p) => names.has(p)) : []);
    if (path !== state.path) {
      ui.filter.value = "";
      ui.body.scrollTop = 0;
      state.message = null;
    }
    state.path = path;
    state.writable = !!b.writable;
    state.entries = entries;
    state.cursor = b.nextCursor || "";
    ui.list.removeAttribute("aria-busy");
    ui.list.replaceChildren(...entries.map(row));
    markRoot();
    renderCrumbs();
    renderMore();
    applyFilter();
    updateTools();
    updateActions();
    keepFocus();
  }

  async function more() {
    if (!state.cursor) return;
    const token = state.token;
    const path = state.path;
    ui.more.disabled = true;
    try {
      const res = await request("GET", "list", { path, cursor: state.cursor });
      if (token !== state.token) return;
      const entries = Array.isArray(res.body.entries) ? res.body.entries : [];
      state.entries.push(...entries);
      state.cursor = res.body.nextCursor || "";
      ui.list.append(...entries.map(row));
    } finally {
      ui.more.disabled = false;
      if (token === state.token) {
        renderMore();
        applyFilter();
        keepFocus();
      }
    }
  }

  function row(entry) {
    const path = state.path + "/" + entry.name;
    const dir = entry.type === "dir";
    const name = el("span", "name", { text: entry.name, title: entry.name });
    let main;
    if (dir) {
      main = el("button", "row-main", { type: "button" }, [icon("dir"), name]);
      main.addEventListener("click", () => navigate(path).catch(() => {}));
    } else {
      const box = el("input", "check", { type: "checkbox" });
      box.checked = state.selected.has(path);
      box.addEventListener("change", () => {
        if (box.checked) state.selected.add(path);
        else state.selected.delete(path);
        li.classList.toggle(P + "selected", box.checked);
        state.message = null;
        updateActions();
      });
      main = el("label", "row-main", null, [
        box,
        icon("file"),
        name,
        el("span", "meta", { text: fmtSize(entry.size), title: entry.size + " bytes" }),
        el("span", "meta date", { text: fmtTime(entry.mtime), title: entry.mtime }),
      ]);
    }
    const li = el("li", "row", { "data-path": path, "data-type": dir ? "dir" : "file" }, [main]);
    if (!dir && state.selected.has(path)) li.classList.add(P + "selected");
    return li;
  }

  function renderMore() {
    if (state.cursor) ui.body.insertBefore(ui.more, ui.empty);
    else ui.more.remove();
  }

  function applyFilter() {
    const needle = ui.filter.value.trim().toLowerCase();
    let shown = 0;
    for (const li of ui.list.children) {
      const hide = !!needle && !baseName(li.dataset.path).toLowerCase().includes(needle);
      li.hidden = hide;
      if (!hide) shown++;
    }
    renderEmpty(shown, needle);
  }

  function renderEmpty(shown, needle) {
    let text = "";
    if (!state.path) {
      if (!host.session) text = "";
      else if (!host.session.roots.some((r) => r.exists && r.id !== "scratch")) text = NO_STORE;
      else text = "Choose a location above to browse your data.";
    } else if (!state.entries.length) text = "This folder is empty.";
    else if (!shown && needle) text = state.cursor ? "No matches in the entries loaded so far." : "No matches.";
    ui.empty.textContent = text;
    ui.empty.hidden = !text;
  }

  function selection() {
    return state.entries.map((e) => state.path + "/" + e.name).filter((p) => state.selected.has(p));
  }

  function setLink(a, href) {
    if (href) {
      a.setAttribute("href", href);
      a.removeAttribute("aria-disabled");
    } else {
      a.removeAttribute("href");
      a.setAttribute("aria-disabled", "true");
    }
  }

  const isProject = (p) => /\.(rgis|rgispkg)$/i.test(p);
  const projectHref = (p) => (/\.rgis$/i.test(p) ? projectLaunchUrl(p) : packageLaunchUrl(p));
  const SHP_PART = /\.(dbf|shx|prj|cpg)$/i;
  const stem = (p) => p.slice(0, p.lastIndexOf("."));

  // What "Open in new map" does with a selection: {href}, or {hint} saying why it is disabled.
  function openPlan(sel) {
    if (!sel.length) return { hint: state.path ? "Select 1 to " + MAX_OPEN + " files to open them in a new map." : "" };
    // Upstream takes a project only through ?project=, which makes it ignore every ?data=.
    if (sel.some(isProject)) {
      return sel.length === 1 ? { href: projectHref(sel[0]) } : { hint: "A project opens on its own: select the project alone, or data files only." };
    }
    // Upstream reads a Shapefile's parts from the .shp address and refuses a part on its own.
    const shapes = new Set(sel.filter((p) => /\.shp$/i.test(p)).map(stem));
    const data = sel.filter((p) => !SHP_PART.test(p) || !shapes.has(stem(p)));
    if (data.some((p) => SHP_PART.test(p))) return { hint: "Select the .shp file; its .dbf, .shx, .prj and .cpg parts are read with it." };
    if (data.length > MAX_OPEN) return { hint: data.length + " files selected; a map opens at most " + MAX_OPEN + " at once." };
    try {
      return { href: launchUrl(data) };
    } catch (e) {
      return { hint: "These paths make the link too long; select fewer files." };
    }
  }

  function updateActions() {
    const sel = selection();
    const plan = openPlan(sel);
    state.hint = plan.hint || "";
    setLink(ui.open, plan.href || null);
    const one = sel.length === 1 ? sel[0] : null;
    setLink(ui.openProject, one && isProject(one) ? projectHref(one) : null);
    ui.copy.disabled = !one;
    ui.count.textContent = sel.length ? sel.length + " selected" : "";
    renderStatus();
  }

  function updateTools() {
    const ok = canWrite();
    const why = ok ? "" : !state.path ? "Open a folder first" : "This location is read-only";
    for (const n of [ui.mkdirName, ui.mkdir, ui.upload]) {
      n.disabled = !ok;
      n.title = why;
    }
  }

  async function copyUrl() {
    const sel = selection();
    if (sel.length !== 1) return;
    const url = fileUrl(sel[0]);
    try {
      await navigator.clipboard.writeText(url);
      setStatus("URL copied. It works in this analysis only (Add Data › From URL).", "ok");
    } catch (e) {
      setStatus("Copy this URL:", "", url);
    }
  }

  async function navigate(path) {
    await dom;
    if (!path || path === "/") {
      state.token++;
      state.path = "";
      state.entries = [];
      state.cursor = "";
      state.selected.clear();
      state.message = null;
      ui.list.replaceChildren();
      ui.list.removeAttribute("aria-busy");
      markRoot();
      renderCrumbs();
      renderMore();
      renderEmpty();
      updateTools();
      updateActions();
      keepFocus();
      return;
    }
    try {
      await ensureSession();
    } catch (e) {
      showError(e);
      throw e;
    }
    await load(String(path), false);
  }

  // ---------------------------------------------------------------- mkdir

  async function mkdir(path) {
    const res = await request("POST", "mkdir", null, { body: JSON.stringify({ path }) });
    if (ui && state.path && path === state.path + "/" + baseName(path)) load(state.path, true).catch(() => {});
    return res.body.entry;
  }

  async function submitMkdir() {
    const name = ui.mkdirName.value.trim();
    if (!name || !canWrite()) {
      ui.mkdirName.focus();
      return;
    }
    if (name.includes("/")) {
      setStatus("A folder name cannot contain a slash", "error");
      return;
    }
    ui.mkdir.disabled = true;
    try {
      await mkdir(state.path + "/" + name);
      ui.mkdirName.value = "";
      setStatus("Created folder “" + name + "”", "ok");
    } catch (e) {
      showError(e);
    } finally {
      updateTools();
    }
  }

  // ---------------------------------------------------------------- upload

  async function uploadId(path, file) {
    let bytes;
    try {
      const text = new TextEncoder().encode(path + "\n" + file.size + "\n" + file.lastModified);
      bytes = new Uint8Array(await crypto.subtle.digest("SHA-256", text)).slice(0, 16);
    } catch (e) {
      // crypto.subtle exists only in secure contexts
      bytes = new Uint8Array(16);
      try {
        crypto.getRandomValues(bytes);
      } catch (err) {
        for (let i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
      }
    }
    return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  }

  function sleep(ms, signal) {
    return new Promise((resolve, reject) => {
      const stop = () => {
        clearTimeout(timer);
        reject(new HostError("aborted", "Cancelled"));
      };
      const timer = setTimeout(() => {
        signal.removeEventListener("abort", stop);
        resolve();
      }, ms);
      if (signal.aborted) stop();
      else signal.addEventListener("abort", stop, { once: true });
    });
  }

  const discard = (item) => (item.id ? request("DELETE", "upload", { path: item.path, id: item.id }).catch(() => {}) : Promise.resolve());

  // The answer to a commit can be lost after the server renamed the file; the retry then meets
  // its own upload under the final name. A file of the right size, written since this transfer
  // began (by the server's clock, 2 s of rounding allowed), is taken to be that upload.
  async function findCommitted(item, since, signal) {
    try {
      const entry = (await request("GET", "stat", { path: item.path }, { signal })).body.entry;
      if (entry && entry.type === "file" && entry.size === item.file.size && Date.parse(entry.mtime) >= since - 2000) return entry;
    } catch (e) {
      if (e.code === "aborted" || e.code === "session_expired") throw e;
    }
    return null;
  }

  // Sends one file; resolves with its entry. Steps are the contract's upload list, 1 to 11.
  async function transfer(item) {
    const file = item.file;
    const size = file.size;
    const limits = (await ensureSession()).limits;
    if (limits.maxUpload && size > limits.maxUpload) {
      throw new HostError("file_too_large", "Larger than the " + fmtSize(limits.maxUpload) + " upload limit", 413);
    }
    if (!item.id) item.id = await uploadId(item.path, file);
    const signal = item.ctrl.signal;
    const q = { path: item.path, id: item.id };
    let chunk = Math.max(MIN_CHUNK, Math.min(limits.maxChunk || MAX_CHUNK, MAX_CHUNK));
    let offset = -1; // unknown: ask the server
    let failures = 0;
    let stalls = 0;
    let since = 0; // the server's clock at this transfer's first answer
    let committing = false;
    let unconfirmed = false; // a commit was sent and no usable answer came back
    for (;;) {
      try {
        if (offset < 0) {
          const head = await request("HEAD", "upload", q, { signal });
          offset = parseInt(head.headers.get("Upload-Offset"), 10) || 0;
          if (!since) since = Date.parse(head.headers.get("Date")) || Date.now();
        }
        if (offset > size) {
          await request("DELETE", "upload", q, { signal });
          offset = 0;
        }
        setProgress(item, offset, "");
        const end = Math.min(offset + chunk, size);
        const params = Object.assign({ offset: String(offset) }, q);
        if (end === size) Object.assign(params, { commit: "1", size: String(size) });
        if (item.overwrite) params.overwrite = "1";
        committing = end === size;
        const res = await request("PUT", "upload", params, { body: file.slice(offset, end), signal });
        committing = false;
        if (typeof res.body.offset !== "number") throw new HostError("http", "Unexpected upload response", res.status);
        offset = res.body.offset;
        failures = 0;
        stalls = 0;
        setProgress(item, offset, "");
        if (res.body.committed) return res.body.entry;
      } catch (e) {
        if (!(e instanceof HostError) || e.code === "aborted" || e.code === "session_expired") throw e;
        if (committing && (e.code === "network" || e.status >= 500)) unconfirmed = true;
        committing = false;
        if (e.status === 401 || e.status === 403) {
          stopUploads(e.message);
          throw e;
        }
        if (e.status === 409 && e.code === "exists") {
          if (item.overwrite) throw e; // a directory has that name
          const done = unconfirmed && (await findCommitted(item, since, signal));
          if (done) return done;
          if (!(await askReplace(item))) {
            await discard(item);
            throw new HostError("skipped", "Skipped");
          }
          item.overwrite = true;
        } else if (e.status === 413 && e.code !== "file_too_large") {
          if (chunk <= MIN_CHUNK) throw e;
          chunk = Math.max(MIN_CHUNK, Math.floor(chunk / 2));
          if (typeof e.offset === "number") offset = e.offset;
        } else if (e.status === 409 && (e.code === "offset_mismatch" || e.code === "size_mismatch")) {
          if (++stalls > 8) throw e;
          offset = typeof e.offset === "number" ? e.offset : -1;
        } else if (e.code === "network" || (e.status >= 500 && e.code !== "no_space")) {
          // irodsfs reports a refused permission or an exceeded quota like any other failure of
          // the store, so store_error is seldom worth more than one more try.
          if (failures >= (e.code === "store_error" ? 1 : RETRY_WAITS.length)) throw e;
          const wait = RETRY_WAITS[failures++];
          setProgress(item, Math.max(offset, 0), e.message + "; retrying in " + wait / 1000 + " s");
          await sleep(wait, signal);
          offset = -1;
        } else {
          throw e;
        }
      }
    }
  }

  function askReplace(item) {
    return new Promise((resolve) => {
      item.answer = (yes) => {
        item.answer = null;
        item.state = "active";
        renderUpload(item);
        resolve(yes);
      };
      item.state = "ask";
      renderUpload(item);
      open();
    });
  }

  function setProgress(item, sent, note) {
    item.sent = sent;
    item.note = note;
    renderUpload(item);
  }

  function settle(item, stateName, note, result, error) {
    item.state = stateName;
    item.note = note;
    item.ctrl = null;
    renderUpload(item);
    if (item.done) {
      if (error) item.done.reject(error);
      else item.done.resolve(result);
      item.done = null;
    }
  }

  async function pump() {
    if (pumping) return;
    pumping = true;
    updateBusy();
    try {
      for (let item; (item = uploads.find((i) => i.state === "queued")); ) {
        item.state = "active";
        item.ctrl = new AbortController();
        renderUpload(item);
        try {
          const entry = await transfer(item);
          settle(item, "done", "Uploaded", entry);
          if (state.path === item.dir) load(item.dir, true).catch(() => {});
        } catch (e) {
          if (e instanceof HostError && e.code === "aborted") {
            await discard(item);
            settle(item, "cancelled", "Cancelled", null);
          } else if (e instanceof HostError && e.code === "skipped") {
            settle(item, "cancelled", "Skipped", null);
          } else {
            settle(item, "failed", (e && e.message) || "Failed", null, e);
          }
        }
      }
    } finally {
      pumping = false;
      updateBusy();
    }
  }

  function stopUploads(reason) {
    for (const item of uploads) {
      if (item.state === "queued") settle(item, "failed", reason, null, new HostError("stopped", reason));
      else if (item.state === "ask" && item.answer) item.answer(false);
    }
  }

  function cancelUpload(item) {
    if (item.state === "queued") settle(item, "cancelled", "Cancelled", null);
    else if (item.state === "ask") item.answer(false);
    else if (item.state === "active") item.ctrl.abort();
    else {
      uploads.splice(uploads.indexOf(item), 1);
      item.el.remove();
      ui.uploads.hidden = !uploads.length;
      updateBusy();
      keepFocus();
    }
  }

  function buildUpload(item) {
    item.bar = el("progress", "", { max: "1", value: "0", "aria-label": "Upload progress for " + item.file.name });
    item.noteEl = el("span", "upload-note");
    item.replace = el("button", "btn small", { type: "button", text: "Replace" });
    item.skip = el("button", "btn small", { type: "button", text: "Skip" });
    item.retry = el("button", "btn small", { type: "button", text: "Retry" });
    item.cancel = el("button", "btn small upload-cancel", { type: "button" });
    item.replace.addEventListener("click", () => item.answer && item.answer(true));
    item.skip.addEventListener("click", () => item.answer && item.answer(false));
    item.retry.addEventListener("click", () => {
      item.state = "queued";
      item.note = "";
      renderUpload(item);
      pump();
    });
    item.cancel.addEventListener("click", () => cancelUpload(item));
    item.el = el("li", "upload-item", null, [
      el("span", "name", { text: item.file.name, title: item.path }),
      item.bar,
      item.noteEl,
      el("span", "upload-buttons", null, [item.replace, item.skip, item.retry, item.cancel]),
    ]);
    ui.uploads.append(item.el);
    ui.uploads.hidden = false;
    renderUpload(item);
  }

  function renderUpload(item) {
    if (!item.el) return;
    const size = item.file.size;
    const live = item.state === "queued" || item.state === "active" || item.state === "ask";
    item.el.dataset.state = item.state;
    item.bar.value = item.state === "done" ? 1 : size ? Math.min(1, item.sent / size) : 0;
    item.noteEl.textContent =
      item.state === "ask"
        ? "A file with this name exists."
        : item.note || (item.state === "queued" ? "Waiting" : fmtSize(item.sent) + " of " + fmtSize(size));
    item.replace.hidden = item.skip.hidden = item.state !== "ask";
    item.retry.hidden = item.state !== "failed" || expired;
    item.cancel.textContent = live ? "Cancel" : "Remove";
    item.cancel.setAttribute("aria-label", (live ? "Cancel upload of " : "Remove from list: ") + item.file.name);
    updateBusy();
  }

  const warnUnload = (e) => {
    e.preventDefault();
    e.returnValue = "";
  };

  function updateBusy() {
    if (!ui) return;
    const busy = uploads.some((i) => i.state === "queued" || i.state === "active" || i.state === "ask");
    ui.root.classList.toggle(P + "busy", busy);
    ui.root.classList.toggle(P + "failed", !busy && uploads.some((i) => i.state === "failed"));
    // Opening a map navigates this tab, which would drop a running upload without a word.
    if (busy) window.addEventListener("beforeunload", warnUnload);
    else window.removeEventListener("beforeunload", warnUnload);
  }

  async function upload(files, dirPath) {
    await dom;
    const dir = String(dirPath || "").replace(/\/+$/, "");
    const items = Array.from(files || []).map((file) => {
      const item = { file, dir, path: dir + "/" + file.name, id: "", state: "queued", sent: 0, note: "", overwrite: false };
      item.promise = new Promise((resolve, reject) => (item.done = { resolve, reject }));
      uploads.push(item);
      buildUpload(item);
      return item;
    });
    pump();
    const results = await Promise.allSettled(items.map((i) => i.promise));
    const failed = results.find((r) => r.status === "rejected");
    if (failed) throw failed.reason;
    return results.map((r) => r.value).filter(Boolean);
  }

  // ---------------------------------------------------------------- drag and drop

  let dropTimer = 0;

  function onDragOver(e) {
    const ok = canWrite() && ui.panel.contains(e.target);
    if (e.dataTransfer) e.dataTransfer.dropEffect = ok ? "copy" : "none";
    if (!ok) return;
    ui.panel.classList.add(P + "dropping");
    clearTimeout(dropTimer);
    dropTimer = setTimeout(() => ui.panel.classList.remove(P + "dropping"), 200);
  }

  function onDrop(e) {
    ui.panel.classList.remove(P + "dropping");
    if (!ui.panel.contains(e.target) || !e.dataTransfer) return;
    if (!canWrite()) {
      setStatus(state.path ? "This location is read-only" : "Open a folder before dropping files", "error");
      return;
    }
    let files = [];
    let folders = 0;
    const items = Array.from(e.dataTransfer.items || []).filter((i) => i.kind === "file");
    for (const it of items) {
      const entry = it.webkitGetAsEntry ? it.webkitGetAsEntry() : null;
      const file = it.getAsFile();
      if (entry && entry.isDirectory) folders++;
      else if (file) files.push(file);
    }
    if (!items.length) files = Array.from(e.dataTransfer.files || []);
    if (folders) setStatus("Folders cannot be uploaded; drop files instead", "error");
    if (files.length) upload(files, state.path).catch(() => {});
  }

  // ---------------------------------------------------------------- bridge seam (M4)

  function detectBridge() {
    const bridge = typeof window.rustgis_host_run === "function" || !!(window.wasmBindings && typeof window.wasmBindings.rustgis_host_run === "function");
    host.mode = bridge ? "bridge" : "launch";
    if (ui) {
      ui.root.classList.toggle(P + "bridge", bridge);
      ui.root.classList.toggle(P + "launch", !bridge);
    }
    return bridge;
  }

  // ---------------------------------------------------------------- start

  const host = {
    version: 1,
    mode: "launch",
    placeholder: PLACEHOLDER,
    session: null,
    ready,
    open,
    close,
    toggle,
    navigate,
    fileUrl,
    projectUrl,
    launchUrl,
    projectLaunchUrl,
    upload,
    mkdir,
  };
  window.rustgisHost = host;

  const dom = new Promise((resolve) => {
    const start = () => {
      build();
      resolve();
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start, { once: true });
    else start();
  });

  window.addEventListener("TrunkApplicationStarted", detectBridge);
  ensureSession().catch(showError);
})();
