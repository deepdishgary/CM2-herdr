// Process B ("Claude daemon stand-in"): opens the SAME vendor collection
// while listener.js already holds it, then issues RPCs:
//   1. device.status  (harmless query, elicits a response)
//   2. v.oai.thstatus (the LED write ChatGPT uses; no agent keys are mapped
//      on the stock keymap, so it ACKs without visible change)
const { CM2 } = require('./device.js');

const t0 = Date.now();
const log = (m) => console.log(`[requester +${Date.now() - t0}ms] ${m}`);

async function main() {
  let dev;
  try {
    dev = await CM2.open();
    log('OPEN OK (nonExclusive, concurrent with listener)');
  } catch (e) {
    log(`OPEN FAILED: ${e.message}`);
    process.exit(3);
  }
  dev.on('error', (e) => log(`HID ERROR: ${e.message}`));

  try {
    const st = await dev.status();
    log(`device.status result: ${JSON.stringify(st)}`);
  } catch (e) { log(`device.status FAILED: ${e.message}`); }

  try {
    const r = await dev.setThreads([{ id: 0, color: 0x0000ff, brightness: 0.3, effect: 'solid' }]);
    log(`v.oai.thstatus (blue) ACK: ${JSON.stringify(r)}`);
  } catch (e) { log(`v.oai.thstatus FAILED: ${e.message}`); }

  dev.close();
  log('closed');
  process.exit(0);
}
main();
