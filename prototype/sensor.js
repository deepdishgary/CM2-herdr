// Open a sensor HID device (non-TCC-gated) and count input reports.
// argv[2] = label, argv[3] = product name, argv[4] = exclusive? "excl"
const HID = require('node-hid');
const label = process.argv[2] || 'proc';
const prod = process.argv[3] || 'accel';
const excl = process.argv[4] === 'excl';
const t0 = Date.now();
const log = (m) => console.log(`[${label} +${Date.now() - t0}ms] ${m}`);

async function main() {
  const info = HID.devices().find(d => d.product === prod);
  if (!info) { log(`no device "${prod}"`); process.exit(2); }
  let hid;
  try {
    hid = await HID.HIDAsync.open(info.path, excl ? {} : { nonExclusive: true });
    log(`OPEN OK (${excl ? 'EXCLUSIVE default' : 'nonExclusive'}) path=${info.path}`);
  } catch (e) { log(`OPEN FAILED: ${e.message}`); process.exit(3); }
  let n = 0, first = null, last = null;
  hid.on('data', (buf) => {
    n++; last = buf.slice(0, 8).toString('hex');
    if (!first) { first = last; log(`first report: ${first} len=${buf.length}`); }
  });
  hid.on('error', (e) => log(`HID ERROR: ${e.message}`));
  setTimeout(async () => {
    log(`TOTAL reports received: ${n}; last8=${last}`);
    await hid.close(); process.exit(0);
  }, 6000);
}
main();
