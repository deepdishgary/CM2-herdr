"use strict";
/**
 * cm2-agent-keys — control the Work Louder Creator Micro 2's per-key
 * "agent" LEDs and receive its key events, over USB or Bluetooth.
 *
 * Clean-room implementation of the device's JSON-RPC-over-HID protocol,
 * written from observed wire behaviour. Contains no vendor code.
 * See PROTOCOL.md for the full protocol description.
 *
 * Requires firmware >= v0.6.0-rc (the "OAI bridge") for per-key methods.
 */
const HID = require("node-hid");
const { EventEmitter } = require("events");

const WL_VENDOR_ID = 0x303a;      // Espressif (the CM2 is ESP32-based)
const VENDOR_USAGE_PAGE = 0xff00; // the RPC channel interface
const REPORT_ID = 0x06;
const CHANNEL_DEBUG = 1;
const CHANNEL_RPC = 2;
const MAX_CHUNK = 61;             // 64-byte report minus 3 header bytes

/** LED animation effects understood by the firmware. */
const Effect = { off: 0, solid: 1, snake: 2, rainbow: 3, breath: 4, gradient: 5 };

/** \uXXXX-escape non-ASCII so every byte on the wire is 7-bit safe. */
function escapeUnicode(s) {
  return s.replace(/[\u0080-\uffff]/g, (c) =>
    "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"));
}

class CM2 extends EventEmitter {
  /** Enumerate connected Creator Micro 2 RPC interfaces (USB or Bluetooth). */
  static list() {
    return HID.devices().filter(
      (d) => d.vendorId === WL_VENDOR_ID &&
             d.usagePage === VENDOR_USAGE_PAGE && d.usage === 1);
  }

  /** Open the first (or a specific) device. Async — uses node-hid's
   * HIDAsync API, which on macOS can open keyboard-class devices where the
   * sync API is refused with a privilege violation. */
  static async open(path) {
    const info = path ? { path } : CM2.list()[0];
    if (!info) throw new Error("no Work Louder device found (USB or Bluetooth)");
    // keyboards can only be opened non-exclusively on macOS — an exclusive
    // open (the default) fails with kIOReturnNotPrivileged
    const hid = process.platform === "darwin"
      ? await HID.HIDAsync.open(info.path, { nonExclusive: true })
      : await HID.HIDAsync.open(info.path);
    return new CM2(hid);
  }

  constructor(hid) {
    super();
    this.hid = hid;
    this._buffers = { [CHANNEL_DEBUG]: "", [CHANNEL_RPC]: "" };
    this._pending = new Map(); // id -> {resolve, reject, timer}
    this._nextId = 1;
    this.hid.on("data", (buf) => this._onReport(buf));
    this.hid.on("error", (err) => this.emit("error", err));
  }

  close() {
    for (const p of this._pending.values()) {
      clearTimeout(p.timer);
      p.reject(new Error("device closed"));
    }
    this._pending.clear();
    try { this.hid.close(); } catch (_) {}
  }

  // ---- transport -----------------------------------------------------

  _onReport(buf) {
    const channel = buf[1];
    const length = buf[2];
    if (!(channel in this._buffers)) return;
    this._buffers[channel] += buf.slice(3, 3 + length).toString("utf8");
    const lines = this._buffers[channel].split(/\r?\n/);
    this._buffers[channel] = lines.pop(); // keep any incomplete tail
    for (const line of lines) {
      if (!line.trim()) continue;
      if (channel === CHANNEL_DEBUG) { this.emit("debug", line); continue; }
      let msg;
      try { msg = JSON.parse(line.slice(line.indexOf("{"))); } catch (_) { continue; }
      this._dispatch(msg);
    }
  }

  _dispatch(msg) {
    if (msg.id !== undefined && this._pending.has(msg.id)) {
      const p = this._pending.get(msg.id);
      this._pending.delete(msg.id);
      clearTimeout(p.timer);
      if (msg.error) p.reject(new Error(msg.error.message || JSON.stringify(msg.error)));
      else p.resolve(msg.result);
      return;
    }
    // device-initiated notification — these use abbreviated field names on
    // the wire: {"m": "v.oai.hid", "p": {"k": "AG00", "act": 1}}
    const method = msg.method || msg.m;
    const params = msg.params || msg.p || {};
    if (method === "v.oai.hid")
      this.emit("key", { key: params.k ?? params.key, act: params.act }); // act 1=press 0=release
    else if (method === "v.oai.rad") this.emit("joystick", params);
    else this.emit("notification", msg);
  }

  /** Send one JSON-RPC request and await its response.
   *
   * Writes are serialised through a queue: concurrent requests would
   * interleave their HID chunks and the device would receive shredded
   * JSON (it accumulates one buffer per channel). */
  async rpc(method, params = null, timeoutMs = 5000) {
    const id = this._nextId++;
    const line = escapeUnicode(JSON.stringify({ method, params, id }));
    const bytes = Buffer.from(line, "utf8");
    const reply = new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this._pending.delete(id);
        reject(new Error(`RPC timeout: ${method}`));
      }, timeoutMs);
      this._pending.set(id, { resolve, reject, timer });
    });
    const write = async () => {
      for (let off = 0; off < bytes.length; off += MAX_CHUNK) {
        const chunk = bytes.slice(off, off + MAX_CHUNK);
        const report = Buffer.alloc(64);
        report[0] = REPORT_ID;
        report[1] = CHANNEL_RPC;
        report[2] = chunk.length;
        chunk.copy(report, 3);
        await this.hid.write(report);
      }
    };
    this._writeQueue = (this._writeQueue || Promise.resolve())
      .catch(() => {})
      .then(write)
      .catch((err) => {
        const p = this._pending.get(id);
        if (p) { clearTimeout(p.timer); this._pending.delete(id); p.reject(err); }
      });
    return reply;
  }

  // ---- high-level API ------------------------------------------------

  /**
   * Set per-key "thread" lighting on agent keys (keymap keycodes
   * KV_OAI_AG00..AG05 -> thread ids 0..5).
   * Accepts friendly fields; only `id` is required, omitted fields keep
   * their current value on the device.
   *   {id, color: 0xRRGGBB, brightness: 0..1, effect: Effect.*|name,
   *    speed: 0..1, syncKeys: bool, syncAmbient: bool}
   */
  setThreads(threads) {
    const wire = threads.map((t) => ({
      id: t.id,
      c: t.color,
      b: t.brightness,
      e: typeof t.effect === "string" ? Effect[t.effect] : t.effect,
      s: t.speed,
      sk: t.syncKeys === undefined ? undefined : t.syncKeys ? 1 : 0,
      sa: t.syncAmbient === undefined ? undefined : t.syncAmbient ? 1 : 0,
    }));
    return this.rpc("v.oai.thstatus", wire);
  }

  /**
   * Configure the two lighting zones: `ambient` (underglow ring) and
   * `keys` (base backlight of all non-agent keys). Each side:
   *   {effect, brightness: 0..1, speed: 0..1, magic, color: 0xRRGGBB}
   */
  setZones({ ambient, keys }) {
    const side = (z) => z && {
      ...z, effect: typeof z.effect === "string" ? Effect[z.effect] : z.effect,
    };
    return this.rpc("v.oai.rgbcfg", { ambient: side(ambient), keys: side(keys) });
  }

  /** Whole-strip preview (works on stock firmware too, never persisted). */
  preview(config) { return this.rpc("lights.preview", config); }

  status() { return this.rpc("device.status"); }
  version() { return this.rpc("sys.version"); }
  enterBootloader() { return this.rpc("sys.bootloader"); }

  listFiles(opts) { return this.rpc("fs.list", opts || null); }
  readFile(file) { return this.rpc("fs.read", { file }); }
  writeFile(file, data) { return this.rpc("fs.write", { file, data }); }

  /** Read and parse the device keymap. */
  async readKeymap() {
    const res = await this.readFile("keymap.json");
    return JSON.parse(typeof res === "string" ? res : res.data);
  }

  /** Write the device keymap (applies live, persists to flash). */
  writeKeymap(keymap) { return this.writeFile("keymap.json", JSON.stringify(keymap)); }
}

module.exports = { CM2, Effect, WL_VENDOR_ID, VENDOR_USAGE_PAGE };
