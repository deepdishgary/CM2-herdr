# Hardware walk: Creator Micro 2 (bead ai-6ky.2, 2026-10-23)

Verified live with the user on this Mac, firmware **0.6.3**, using `bin/cm2probe` (branch `feature/device-probe`).
Everything here was observed on the real pad unless marked otherwise. Earlier facts in `docs/design.md`
("Device facts") that this corrects are noted.

## USB and transport

- macOS shows **three** IOHIDDevices for 303A:8298 (keyboard 0x0001/0x06, consumer 0x000C/0x01, vendor 0xFF00/0x01),
  not one composite device. cm2d opens the vendor one, shared (`kIOHIDOptionsTypeNone`).
- Vendor reports: report id 6, 64 bytes including the id (id, channel, length, up to 61 bytes of JSON).
- Requests need **no newline**. Field order `method, params, id` works. Replies **echo the method**:
  `{"result":{...},"id":21437,"method":"sys.version"}`. A parser must treat "has result/error" as a reply even when
  `method` is present (this was a real bug in the first build).
- The Claude desktop app already holds the Input Monitoring grant, so tools run from Claude sessions can open the pad.
  cm2d under launchd will need its own grant.
- Two clients on the pad at once garble each other's replies. The Input app must be closed (it starts at login);
  ChatGPT's Input Monitoring is off (2026-10-23).

## Device RPC

| Method | Observed |
|---|---|
| `sys.version` | `{"version":"0.6.3"}` |
| `device.status` | `{"version","profile_index":0,"layer_index":1,"battery":100,"is_charging"}`. `layer_index` is **1-based**: `1` = `layers[0]`. |
| `fs.read {"file":"keymap.json"}` | `{"data":"<keymap JSON as a string>"}`, 1813 bytes, 37 frames. Two reads were byte-identical. |
| `fs.write {"file":"keymap.json","data":"<json>"}` | `null` result; applies live; read-back matched every time (5 writes). |
| `v.oai.thstatus [ ... ]` | always `{"ok":1}`, even for payloads that show nothing. Verify by eye. |

There is no layer-switch RPC and no layer-change notification (okko/micro2-agent-keys; consistent with what we saw).

## Keymap and layers

- `keymap.json`: `profiles[activeProfileId].layers[]`, each with `layout.keymap` rows `[2, 4, 4, 3]`,
  `layout.encoders [[cw, ccw, press]]`, `layout.joystick`, and `lights`.
- **Layer switching** uses Input's "Layer Keys": `KI_LS1`…`KI_LS6` select layer 1…6; `KI_LM1`…`KI_LM6` are the
  momentary versions (untested). Installed: dial press = `KI_LS2` on Layer 1 and `KI_LS1` on Layer 2, so a dial press
  toggles between them. Verified: the press switched layers both ways; it emits no vendor event.
- Layer 1 (id 0) keeps the user's bindings; only its dial press changed (was `KC_MPLY`, play/pause).
- The layout lives in `config/layout.json`; `null` cells keep whatever the pad has.
  `cm2probe keymap apply|restore|show` backs up before every write and verifies after.

## Agent layer (Layer 2, id 1): keys

Physical layout (the small circle at bottom-left is the Work Louder logo, not a key; three tiny LEDs sit to its left,
one lit blue on Layer 2, probably a layer indicator, unverified):

```
 (dial)  [AG00] [AG01]  (joy)
 [AG02] [AG03] [AG04] [AG05]
 [AG06] [AG07] [AG08] [AG09]
   o    [ AG10 + AG11 ] [AG12]
```

- Key events: `{"m":"v.oai.hid","p":{"k":"AG03","act":1}}` on press, `act:0` on release. Rows are in reading order
  (left to right, top row not reversed). Two full passes matched.
- **The wide key is two switches.** One press reports `AG11` and `AG10` about 5 ms apart (either order), and each half
  has its own light. cm2d treats AG10/AG11 as one APPROVE control and lights both.
- **AG12 (bottom-right) has a light** (Dispatch reported thread 12 dark on its pad; not so here).
- Dial on Layer 2: `KV_OAI_ENC_CW` / `KV_OAI_ENC_CC` emit `{"k":"ENC_CW","act":2}` per detent (one event per click);
  encoder slot 0 = right turn (CW). Verified: 6 right clicks -> 6 `ENC_CW`.
- Joystick on Layer 1 (type RADIAL) emits `kb.radial {"a":angle,"d":deflection,"s":sector,"l":layer,...}` (~40/s while
  moving), where `l` is the 0-based active layer. Unassigned on Layer 2 in v1.

## Agent layer: lights

- Entry: `{"id":N,"c":0xRRGGBB,"b":0..1,"e":effect,"s":speed}`. Id N lights the key bound to `KV_OAI_AG<NN>`.
- **Brightness is 0..1.** `b:0.2` was visibly dim; `b:50` showed as full (clamped).
- Effects: `0` off, `1` solid, `4` breathing, `6` shallow breathing. **Breathing needs `s`**: without it the key
  stayed dark; with `s:0.5` or `s:1` it pulsed slowly between full and dark.
- A layer with agent keycodes ignores its normal backlight; keys with no thread state stay dark.
- Thread state persists across layer switches: a pattern sent while Layer 1 was active appeared when the user switched
  to Layer 2.
- Colors used in the test: blue `255`, green `65280`, red `16711680`, amber `16752640` (0xFFA000) all rendered.

## Backups

- Original keymap: `backups/keymap-2026-10-23-a.json` (git-ignored) and `~/Backups/cm2-keymap-2026-10-23/`.
  sha256 `bd1e5d749ce6f3e74de7fc79a82b44c2d37290d62e7af99b37a41f9a13f747f0`.
- `cm2probe keymap restore backups/keymap-2026-10-23-a.json` puts it back (tested live, then the layout was re-applied).
