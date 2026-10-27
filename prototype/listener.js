// Process A ("ChatGPT stand-in"): opens the vendor collection non-exclusively
// and logs every raw RPC-channel line it receives. Holds the device open.
const HID = require('node-hid');

const t0 = Date.now();
const log = (m) => console.log(`[listener +${Date.now() - t0}ms] ${m}`);

async function main() {
  const info = HID.devices().find(d =>
    d.vendorId === 0x303a && d.productId === 0x8298 &&
    d.usagePage === 0xff00 && d.usage === 1);
  if (!info) { log('NO DEVICE'); process.exit(2); }

  let hid;
  try {
    hid = await HID.HIDAsync.open(info.path, { nonExclusive: true });
    log('OPEN OK (nonExclusive)');
  } catch (e) {
    log(`OPEN FAILED: ${e.message}`);
    process.exit(3);
  }

  const buffers = { 1: '', 2: '' };
  hid.on('data', (buf) => {
    const ch = buf[1], len = buf[2];
    if (!(ch in buffers)) return;
    buffers[ch] += buf.slice(3, 3 + len).toString('utf8');
    const lines = buffers[ch].split(/\r?\n/);
    buffers[ch] = lines.pop();
    for (const line of lines) {
      if (line.trim()) log(`RX ch${ch}: ${line.trim()}`);
    }
  });
  hid.on('error', (e) => log(`HID ERROR: ${e.message}`));

  // Hold open for 12 seconds, then exit.
  setTimeout(async () => { log('closing'); await hid.close(); process.exit(0); }, 12000);
}
main();
